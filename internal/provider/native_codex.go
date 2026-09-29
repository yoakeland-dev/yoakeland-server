package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	case agent.ModeAuto: // Codex's reviewer (codexReviewer) answers what it would ask
		return "workspace-write", "on-request"
	case agent.ModeReadOnly, agent.ModePlan:
		return "read-only", "never"
	default: // Ask and Accept Edits: Codex asks either way; the bridge approves the edits
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

// codexSession is one live `codex app-server`: one thread at a time, turns
// started on it as they come. Its notifications go to the turn in progress and
// are dropped between turns.
type codexSession struct {
	proc *nativeProc
	conn *rpcConn

	mu     sync.Mutex
	turn   *codexTurn
	thread string // the thread the next turn continues
}

func (s *codexSession) dead() bool {
	select {
	case <-s.conn.done:
		return true
	default:
		return false
	}
}

func (s *codexSession) close() {
	_ = s.proc.stdin.Close()
	select {
	case <-s.conn.done:
	case <-time.After(3 * time.Second):
		if s.proc.kill != nil {
			s.proc.kill()
		}
	}
}

func (s *codexSession) setTurn(t *codexTurn) {
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
}

func (s *codexSession) current() *codexTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *codexSession) onNotify(method string, params json.RawMessage) {
	if t := s.current(); t != nil {
		t.notify(method, params)
	}
}

func (s *codexSession) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	if t := s.current(); t != nil {
		t.request(id, method, params)
		return
	}
	s.conn.respondError(id, -32000, "no turn is running")
}

func (p *CLISubprocessProvider) startCodex() (*codexSession, error) {
	// The process outlives the turn that started it, so it isn't bound to the turn's context.
	proc, err := p.start()(context.Background(), "codex", []string{"app-server", "-c", `web_search="live"`, "-c", `model_reasoning_summary="detailed"`}, p.Dir)
	if err != nil {
		return nil, err
	}
	s := &codexSession{proc: proc}
	s.conn = newRPCConn(proc, s.onNotify, s.onRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.conn.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "yoakeland", "version": "1"}, "capabilities": nil}); err != nil {
		s.close()
		return nil, err
	}
	s.conn.notify("initialized", nil)
	return s, nil
}

// codexTurn is what one turn has seen of the session's output so far.
type codexTurn struct {
	p    *CLISubprocessProvider
	s    *codexSession
	ctx  context.Context
	nt   *agent.NativeTurn
	emit func(agent.ModelStreamChunk)
	done chan json.RawMessage // the turn object of turn/completed

	mu        sync.Mutex
	changes   map[string][]*diff.FileDiff // fileChange item id -> its diffs, for its approval prompt
	usage     protocol.UsageData
	lastError string
}

func (t *codexTurn) notify(method string, params json.RawMessage) {
	emit := t.emit
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
			t.mu.Lock()
			t.changes[n.Item.ID] = codexDiffs(n.Item)
			t.mu.Unlock()
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
					InputTokens       int `json:"inputTokens"`
					CachedInputTokens int `json:"cachedInputTokens"`
					OutputTokens      int `json:"outputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &n) == nil {
			t.mu.Lock()
			t.usage.InputTokens += n.TokenUsage.Last.InputTokens
			t.usage.OutputTokens += n.TokenUsage.Last.OutputTokens
			t.usage.CachedInputTokens += n.TokenUsage.Last.CachedInputTokens
			t.mu.Unlock()
		}
	case "error":
		var n struct {
			Error     struct{ Message string }
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(params, &n) == nil && !n.WillRetry {
			t.mu.Lock()
			t.lastError = n.Error.Message
			t.mu.Unlock()
		}
	case "account/rateLimits/updated":
		var n struct {
			RateLimits json.RawMessage `json:"rateLimits"`
		}
		if json.Unmarshal(params, &n) == nil {
			if rl, err := parseCodexRateLimits(n.RateLimits); err == nil {
				t.p.reportRateLimits(rl)
			}
		}
	case "turn/completed":
		var n struct{ Turn json.RawMessage }
		_ = json.Unmarshal(params, &n)
		select {
		case t.done <- n.Turn:
		default:
		}
	}
}

func (t *codexTurn) request(id json.RawMessage, method string, params json.RawMessage) {
	conn, nt, ctx := t.s.conn, t.nt, t.ctx
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
		CallID    string `json:"callId"` // legacy execCommandApproval/applyPatchApproval
		GrantRoot string `json:"grantRoot"`
	}
	_ = json.Unmarshal(params, &r)
	ask := func(toolName, desc string, d *diff.FileDiff, edit bool) (agent.NativeAnswer, error) {
		callID := r.ItemID
		if callID == "" {
			callID = r.CallID
		}
		if r.Reason != "" {
			desc += " — " + r.Reason
		}
		return nt.Ask(ctx, agent.NativeAsk{ToolCallID: callID, ToolName: toolName, Description: desc, Diff: d, Edit: edit})
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
		conn.respond(id, map[string]any{"decision": decision(ask("shell", r.Command, nil, false))})
	case "item/fileChange/requestApproval":
		t.mu.Lock()
		ds := t.changes[r.ItemID]
		t.mu.Unlock()
		var paths []string
		var first *diff.FileDiff
		for _, d := range ds {
			paths = append(paths, d.FilePath)
		}
		if len(ds) > 0 {
			first = ds[0]
		}
		conn.respond(id, map[string]any{"decision": decision(ask("apply_patch", "Edit "+strings.Join(paths, ", "), first, r.GrantRoot == ""))})
	case "execCommandApproval", "applyPatchApproval":
		legacy := map[string]string{"accept": "approved", "acceptForSession": "approved_for_session", "decline": "denied", "cancel": "abort"}
		d := decision(ask(method, r.Command, nil, false))
		if d == "decline" {
			conn.respond(id, map[string]any{"decision": map[string]any{"denied": map[string]string{"rejection": "The user denied this."}}})
			return
		}
		conn.respond(id, map[string]any{"decision": legacy[d]})
	case "item/tool/requestUserInput":
		qs := make([]agent.NativeQuestion, len(r.Questions))
		for i, q := range r.Questions {
			qs[i] = agent.NativeQuestion{ToolCallID: r.ItemID + "#" + q.ID, Question: q.Question}
			for _, o := range q.Options {
				qs[i].Options = append(qs[i].Options, o.Label)
			}
		}
		got, err := nt.Questions(ctx, qs)
		if err != nil {
			conn.respondError(id, -32000, "cancelled")
			return
		}
		answers := map[string]any{}
		for i, q := range r.Questions {
			answers[q.ID] = map[string]any{"answers": []string{got[i]}}
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

// codexReviewer is who answers Codex's approval requests: the user, or in Auto
// its own reviewer. It lasts across turns, so every turn says which.
func codexReviewer(m agent.Mode) string {
	if m == agent.ModeAuto {
		return "auto_review"
	}
	return "user"
}

// codexSandboxPolicy is codexPolicy's sandbox as the tagged object turn/start takes.
func codexSandboxPolicy(sandbox string) map[string]any {
	switch sandbox {
	case "read-only":
		return map[string]any{"type": "readOnly"}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	default:
		return map[string]any{"type": "workspaceWrite"}
	}
}

func (p *CLISubprocessProvider) runCodex(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	err := p.runCodexOnce(ctx, nt, emit)
	if errors.Is(err, errStaleSession) {
		err = p.runCodexOnce(ctx, nt, emit)
	}
	return err
}

func (p *CLISubprocessProvider) runCodexOnce(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	ns, reused, release, err := acquireSession(nt, CLIKindCodex, p.Dir, func(nativeSession) bool { return true },
		func() (nativeSession, error) { return p.startCodex() })
	if err != nil {
		return err
	}
	defer release()
	s := ns.(*codexSession)
	// A call that failed because a reused process was already gone is worth one retry.
	failed := func(err error) error {
		if reused && s.dead() && ctx.Err() == nil {
			return errStaleSession
		}
		return err
	}

	t := &codexTurn{p: p, s: s, ctx: ctx, nt: nt, emit: emit, done: make(chan json.RawMessage, 1), changes: map[string][]*diff.FileDiff{}}
	s.setTurn(t)
	defer s.setTurn(nil)
	conn := s.conn

	sandbox, approval := codexPolicy(nt.Mode)
	threadParams := map[string]any{"cwd": p.Dir, "sandbox": sandbox, "approvalPolicy": approval, "approvalsReviewer": codexReviewer(nt.Mode)}
	if p.Model != "" {
		threadParams["model"] = p.Model
	}
	prompt := nt.Prompt
	s.mu.Lock()
	threadID := s.thread
	s.mu.Unlock()
	if nt.ResumeID == "" || nt.ResumeID != threadID {
		threadID = ""
		var thread json.RawMessage
		if nt.ResumeID != "" {
			threadParams["threadId"] = nt.ResumeID
			thread, err = conn.call(ctx, "thread/resume", threadParams)
			if err != nil {
				if s.dead() {
					return failed(err)
				}
				// The rollout is gone (deleted, other machine): start over from
				// the transcript rather than failing the turn.
				delete(threadParams, "threadId")
				prompt = nt.FreshPrompt
				thread = nil
			}
		}
		if thread == nil {
			if thread, err = conn.call(ctx, "thread/start", threadParams); err != nil {
				return failed(err)
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
		threadID = ts.Thread.ID
		s.mu.Lock()
		s.thread = threadID
		s.mu.Unlock()
	}
	nt.SessionID = threadID

	// The policy, model and effort ride on every turn, so changing one keeps the thread.
	turnParams := map[string]any{
		"threadId":       threadID,
		"input":          []map[string]any{{"type": "text", "text": planPrefix(nt.Mode) + prompt, "text_elements": []any{}}},
		"approvalPolicy": approval,
		"sandboxPolicy":  codexSandboxPolicy(sandbox),

		"approvalsReviewer": codexReviewer(nt.Mode),
	}
	if p.Model != "" {
		turnParams["model"] = p.Model
	}
	if p.effort != "" {
		turnParams["effort"] = p.effort
	}
	if p.fast {
		turnParams["serviceTier"] = "fast"
	}
	started, err := conn.call(ctx, "turn/start", turnParams)
	if err != nil {
		if ctx.Err() != nil {
			// We can't know whether it started; a clean process is the safe state.
			s.close()
			return ctx.Err()
		}
		return failed(err)
	}
	var st struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(started, &st)

	// turn/steer names the turn it expects to be running, so a message can't
	// land in a turn that has just ended.
	var over atomic.Bool
	defer over.Store(true)
	nt.SetSteer(func(text string) error {
		if over.Load() {
			return errors.New("the turn has ended")
		}
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := conn.call(c, "turn/steer", map[string]any{
			"threadId": threadID, "expectedTurnId": st.Turn.ID,
			"input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}},
		})
		return err
	})

	var turn struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	select {
	case raw := <-t.done:
		_ = json.Unmarshal(raw, &turn)
	case <-conn.done:
		select {
		case raw := <-t.done:
			_ = json.Unmarshal(raw, &turn)
		default:
			return conn.exitErr("turn/start")
		}
	case <-ctx.Done():
		// Stop the turn, not the process: it stays usable for the next one.
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = conn.call(stop, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": st.Turn.ID})
		cancel()
		select {
		case <-t.done:
		case <-conn.done:
		case <-time.After(5 * time.Second):
			s.close()
		}
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

	t.mu.Lock()
	u := t.usage
	msg := t.lastError
	t.mu.Unlock()
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
