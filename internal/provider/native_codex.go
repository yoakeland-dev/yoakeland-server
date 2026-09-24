package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
)

// Codex, driven through `codex app-server` (SPEC.md §6b) — its documented
// JSON-RPC interface, and the only one with approvals, streamed deltas and
// quota: `codex exec --json` has none of those. Types come from
// `codex app-server generate-ts`. Per turn: initialize, thread/start (or
// thread/resume), turn/start, then relay item/* notifications and answer
// approval requests until turn/completed, then read account/rateLimits.

// codexPolicy maps yoakeland's mode onto Codex's sandbox + approval policy.
// Autonomous stays sandboxed to the workspace, just without asking — the
// same "no prompts, still confined" meaning it has for yoakeland's own bash.
func codexPolicy(m agent.Mode) (sandbox, approval string) {
	switch m {
	case agent.ModeAutonomous:
		return "workspace-write", "never"
	case agent.ModeReadOnly, agent.ModePlan:
		return "read-only", "never"
	default:
		return "workspace-write", "untrusted"
	}
}

type codexItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Command          string          `json:"command"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Status           string          `json:"status"`
	Changes          []codexChange   `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query        string          `json:"query"`
	ContentItems json.RawMessage `json:"contentItems"`
	Success      *bool           `json:"success"`
}

type codexChange struct {
	Path string `json:"path"`
	Kind struct {
		Type string `json:"type"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

// codexToolCall describes a tool-ish thread item, or reports false for items
// that aren't tool calls (messages, reasoning, plans).
func codexToolCall(it codexItem) (agent.ToolCall, bool) {
	raw, _ := json.Marshal(it)
	tc := agent.ToolCall{ID: it.ID, Name: it.Type, Args: raw}
	tc.Native = true
	switch it.Type {
	case "commandExecution":
		tc.Kind, tc.Title = "execute", it.Command
		tc.Args = agent.MarshalArgs(map[string]string{"command": it.Command})
	case "fileChange":
		tc.Kind = "edit"
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		tc.Title, tc.Locations = "Edit "+strings.Join(paths, ", "), paths
	case "mcpToolCall":
		tc.Kind, tc.Title, tc.Args = "other", it.Server+"/"+it.Tool, it.Arguments
	case "dynamicToolCall":
		tc.Kind, tc.Title, tc.Args = "other", it.Tool, it.Arguments
	case "webSearch":
		tc.Kind, tc.Title = "fetch", strings.TrimSpace("Search "+it.Query)
	default:
		return tc, false
	}
	if len(tc.Args) == 0 {
		tc.Args = json.RawMessage("{}")
	}
	return tc, true
}

func codexDiffs(it codexItem) []*diff.FileDiff {
	var out []*diff.FileDiff
	for _, c := range it.Changes {
		out = append(out, parseUnifiedDiff(c.Path, c.Diff))
	}
	return out
}

func codexResult(it codexItem) *agent.NativeToolResult {
	r := &agent.NativeToolResult{ToolCallID: it.ID, Name: it.Type}
	switch it.Type {
	case "commandExecution":
		if it.AggregatedOutput != nil {
			r.Output = *it.AggregatedOutput
		}
		if it.ExitCode != nil && *it.ExitCode != 0 {
			r.Error = fmt.Sprintf("\n[exit code %d]", *it.ExitCode)
		}
	case "fileChange":
		r.Diffs = codexDiffs(it)
		r.Output = it.Status
	case "mcpToolCall":
		r.Output = string(it.Result)
		if it.Error != nil {
			r.Error = it.Error.Message
		}
	case "dynamicToolCall":
		r.Output = string(it.ContentItems)
	}
	if it.Status == "failed" || it.Status == "declined" {
		if r.Error == "" {
			r.Error = it.Status
		}
	}
	return r
}

func (p *CLISubprocessProvider) runCodex(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	proc, err := p.start()(ctx, "codex", []string{"app-server", "-c", `web_search="live"`}, p.Dir)
	if err != nil {
		return err
	}
	defer func() { _ = proc.stdin.Close() }()

	var (
		mu        sync.Mutex
		changes   = map[string][]*diff.FileDiff{} // fileChange item id -> its diffs, for its approval prompt
		usage     protocol.UsageData
		turnDone  = make(chan json.RawMessage, 1)
		lastError string
	)
	var conn *rpcConn
	onNotify := func(method string, params json.RawMessage) {
		switch method {
		case "item/agentMessage/delta":
			var d struct{ Delta string }
			_ = json.Unmarshal(params, &d)
			emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: d.Delta})
		case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
			var d struct{ Delta string }
			_ = json.Unmarshal(params, &d)
			emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: d.Delta})
		case "item/started", "item/completed":
			var n struct{ Item codexItem }
			if json.Unmarshal(params, &n) != nil {
				return
			}
			tc, isTool := codexToolCall(n.Item)
			if !isTool {
				return
			}
			if n.Item.Type == "fileChange" {
				mu.Lock()
				changes[n.Item.ID] = codexDiffs(n.Item)
				mu.Unlock()
			}
			if method == "item/started" {
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
			} else {
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: codexResult(n.Item)})
			}
		case "thread/tokenUsage/updated":
			// `last` is one model request's usage; a turn makes several.
			var n struct {
				TokenUsage struct {
					Last struct {
						InputTokens  int `json:"inputTokens"`
						OutputTokens int `json:"outputTokens"`
					} `json:"last"`
				} `json:"tokenUsage"`
			}
			if json.Unmarshal(params, &n) == nil {
				mu.Lock()
				usage.InputTokens += n.TokenUsage.Last.InputTokens
				usage.OutputTokens += n.TokenUsage.Last.OutputTokens
				mu.Unlock()
			}
		case "error":
			var n struct {
				Error     struct{ Message string }
				WillRetry bool `json:"willRetry"`
			}
			if json.Unmarshal(params, &n) == nil && !n.WillRetry {
				mu.Lock()
				lastError = n.Error.Message
				mu.Unlock()
			}
		case "account/rateLimits/updated":
			var n struct {
				RateLimits json.RawMessage `json:"rateLimits"`
			}
			if json.Unmarshal(params, &n) == nil {
				if rl, err := parseCodexRateLimits(n.RateLimits); err == nil {
					p.reportRateLimits(rl)
				}
			}
		case "turn/completed":
			var n struct{ Turn json.RawMessage }
			_ = json.Unmarshal(params, &n)
			select {
			case turnDone <- n.Turn:
			default:
			}
		}
	}
	onRequest := func(id json.RawMessage, method string, params json.RawMessage) {
		var r struct {
			ItemID    string `json:"itemId"`
			Command   string `json:"command"`
			Reason    string `json:"reason"`
			Questions []struct {
				ID       string `json:"id"`
				Question string `json:"question"`
				Options  []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
			CallID string `json:"callId"` // legacy execCommandApproval/applyPatchApproval
		}
		_ = json.Unmarshal(params, &r)
		ask := func(toolName, desc string, d *diff.FileDiff) (agent.NativeAnswer, error) {
			callID := r.ItemID
			if callID == "" {
				callID = r.CallID
			}
			if r.Reason != "" {
				desc += " — " + r.Reason
			}
			return nt.Ask(ctx, agent.NativeAsk{ToolCallID: callID, ToolName: toolName, Description: desc, Diff: d})
		}
		decision := func(ans agent.NativeAnswer, err error) string {
			switch {
			case err != nil:
				return "cancel"
			case ans.Allowed && ans.Remember:
				return "acceptForSession"
			case ans.Allowed:
				return "accept"
			default:
				return "decline"
			}
		}
		switch method {
		case "item/commandExecution/requestApproval":
			conn.respond(id, map[string]any{"decision": decision(ask("shell", r.Command, nil))})
		case "item/fileChange/requestApproval":
			mu.Lock()
			ds := changes[r.ItemID]
			mu.Unlock()
			var paths []string
			var first *diff.FileDiff
			for _, d := range ds {
				paths = append(paths, d.FilePath)
			}
			if len(ds) > 0 {
				first = ds[0]
			}
			conn.respond(id, map[string]any{"decision": decision(ask("apply_patch", "Edit "+strings.Join(paths, ", "), first))})
		case "execCommandApproval", "applyPatchApproval":
			legacy := map[string]string{"accept": "approved", "acceptForSession": "approved_for_session", "decline": "denied", "cancel": "abort"}
			d := decision(ask(method, r.Command, nil))
			if d == "decline" {
				conn.respond(id, map[string]any{"decision": map[string]any{"denied": map[string]string{"rejection": "The user denied this."}}})
				return
			}
			conn.respond(id, map[string]any{"decision": legacy[d]})
		case "item/tool/requestUserInput":
			answers := map[string]any{}
			for _, q := range r.Questions {
				var opts []string
				for _, o := range q.Options {
					opts = append(opts, o.Label)
				}
				ans, err := nt.Question(ctx, r.ItemID+"#"+q.ID, q.Question, opts)
				if err != nil {
					conn.respondError(id, -32000, "cancelled")
					return
				}
				answers[q.ID] = map[string]any{"answers": []string{ans}}
			}
			conn.respond(id, map[string]any{"answers": answers})
		case "item/permissions/requestApproval":
			// Extra sandbox permissions for the rest of the turn; granting
			// them isn't something a per-tool-call prompt can express.
			conn.respond(id, map[string]any{"permissions": map[string]any{}, "scope": "turn"})
		case "mcpServer/elicitation/request":
			conn.respond(id, map[string]any{"action": "decline", "content": nil, "_meta": nil})
		default:
			conn.respondError(id, -32601, "yoakeland doesn't handle "+method)
		}
	}
	conn = newRPCConn(proc, onNotify, onRequest)

	if _, err := conn.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "yoakeland", "version": "1"}, "capabilities": nil}); err != nil {
		return err
	}
	conn.notify("initialized", nil)

	sandbox, approval := codexPolicy(nt.Mode)
	threadParams := map[string]any{"cwd": p.Dir, "sandbox": sandbox, "approvalPolicy": approval}
	if p.Model != "" {
		threadParams["model"] = p.Model
	}
	var thread json.RawMessage
	prompt := nt.Prompt
	if nt.ResumeID != "" {
		threadParams["threadId"] = nt.ResumeID
		thread, err = conn.call(ctx, "thread/resume", threadParams)
		if err != nil {
			// The rollout is gone (deleted, other machine): start over from
			// the transcript rather than failing the turn.
			delete(threadParams, "threadId")
			prompt = nt.FreshPrompt
			thread = nil
		}
	}
	if thread == nil {
		if thread, err = conn.call(ctx, "thread/start", threadParams); err != nil {
			return err
		}
	}
	var ts struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(thread, &ts); err != nil || ts.Thread.ID == "" {
		return fmt.Errorf("codex thread/start: no thread id")
	}
	nt.SessionID = ts.Thread.ID

	turnParams := map[string]any{
		"threadId": ts.Thread.ID,
		"input":    []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}},
	}
	if p.effort != "" {
		turnParams["effort"] = p.effort
	}
	started, err := conn.call(ctx, "turn/start", turnParams)
	if err != nil {
		return err
	}
	var st struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(started, &st)

	var turn struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	select {
	case raw := <-turnDone:
		_ = json.Unmarshal(raw, &turn)
	case <-conn.done:
		return conn.exitErr("turn/start")
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = conn.call(stop, "turn/interrupt", map[string]any{"threadId": ts.Thread.ID, "turnId": st.Turn.ID})
		cancel()
		return ctx.Err()
	}

	// Quota costs no model usage to read; a failure just keeps the last value.
	rlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if raw, err := conn.call(rlCtx, "account/rateLimits/read", nil); err == nil {
		var r struct {
			RateLimits json.RawMessage `json:"rateLimits"`
		}
		if json.Unmarshal(raw, &r) == nil {
			if rl, err := parseCodexRateLimits(r.RateLimits); err == nil {
				p.reportRateLimits(rl)
			}
		}
	}
	cancel()

	mu.Lock()
	u := usage
	msg := lastError
	mu.Unlock()
	u.TotalTokens = u.InputTokens + u.OutputTokens
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &u}) // no cost reported; the agent estimates it
	if turn.Status == "failed" {
		if turn.Error != nil && turn.Error.Message != "" {
			msg = turn.Error.Message
		}
		return fmt.Errorf("codex: %s", msg)
	}
	return nil
}
