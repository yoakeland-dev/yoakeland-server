package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"yoakeland/internal/agent"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
)

// Claude Code, driven through its stream-json mode (SPEC.md §6b):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//	  --include-partial-messages --permission-prompt-tool stdio
//	  --permission-mode <mode> [--model m] [--effort e] [--resume id]
//
// The prompt goes in as one `user` line on stdin, which stays open so the
// CLI's permission asks (`control_request` can_use_tool — the protocol the
// Agent SDK's canUseTool callback rides on; `stdio` as the prompt tool is the
// undocumented piece) can be answered on it. stdin is closed once `result`
// arrives, which ends the process.

// claudePermissionMode maps yoakeland's mode onto Claude Code's own. read-only
// has no exact equivalent; plan is Claude's read-only mode. Asks that still
// reach us are answered by agent.askNativePermission, which enforces the
// yoakeland mode regardless.
func claudePermissionMode(m agent.Mode) string {
	switch m {
	case agent.ModeAutonomous:
		return "bypassPermissions"
	case agent.ModeReadOnly, agent.ModePlan:
		return "plan"
	default:
		return "default"
	}
}

// claudeAutonomousSandbox keeps bypassPermissions "no prompts, still
// confined", like yoakeland's own autonomous bash and Codex's
// workspace-write: Claude Code's own OS sandbox (bubblewrap/Seatbelt) for
// every Bash command, no unsandboxed retry, and a refusal to start rather
// than a silent unsandboxed fallback when bubblewrap is missing.
// ponytail: covers Claude's Bash only — its Edit/Write tools still follow the
// permission mode; upgrade path is running the whole CLI under bwrap.
const claudeAutonomousSandbox = `{"sandbox":{"enabled":true,"failIfUnavailable":true,"allowUnsandboxedCommands":false}}`

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeLine struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Event           struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		Content []claudeBlock `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"tool_use_result"`
	RequestID     string          `json:"request_id"`
	Request       json.RawMessage `json:"request"`
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`

	// result
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *CLISubprocessProvider) runClaude(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", claudePermissionMode(nt.Mode)}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	if p.effort != "" {
		args = append(args, "--effort", p.effort)
	}
	if nt.ResumeID != "" {
		args = append(args, "--resume", nt.ResumeID)
	}
	if nt.Mode == agent.ModeAutonomous {
		args = append(args, "--settings", claudeAutonomousSandbox)
	}
	proc, err := p.start()(ctx, "claude", args, p.Dir)
	if err != nil {
		return err
	}
	var wmu sync.Mutex
	write := func(v any) { _ = writeJSONLine(proc.stdin, &wmu, v) }
	write(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": nt.Prompt}}},
		"parent_tool_use_id": nil,
	})

	var (
		names    = map[string]string{}           // tool_use_id -> tool name
		proposed = map[string][]*diff.FileDiff{} // tool_use_id -> diff computed before it ran
		res      *claudeLine
		mu       sync.Mutex
	)
	for proc.lines.Scan() {
		var ln claudeLine
		if json.Unmarshal(proc.lines.Bytes(), &ln) != nil {
			continue
		}
		if ln.SessionID != "" && nt.SessionID == "" {
			nt.SessionID = ln.SessionID
		}
		topLevel := ln.ParentToolUseID == nil || *ln.ParentToolUseID == ""
		switch ln.Type {
		case "stream_event":
			if topLevel && ln.Event.Type == "content_block_delta" {
				switch ln.Event.Delta.Type {
				case "text_delta":
					emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: ln.Event.Delta.Text})
				case "thinking_delta":
					emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: ln.Event.Delta.Thinking})
				}
			}
		case "assistant":
			// Text already streamed as deltas; only the tool calls are new.
			// A subagent's (Task) own calls carry the Task's id as parent.
			for _, b := range ln.Message.Content {
				if b.Type != "tool_use" {
					continue
				}
				mu.Lock()
				names[b.ID] = b.Name
				d := claudeProposedDiffs(b.Name, b.Input)
				proposed[b.ID] = d
				mu.Unlock()
				tc := claudeToolCall(b.ID, b.Name, b.Input)
				if !topLevel {
					tc.ParentToolCallID = *ln.ParentToolUseID
				}
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
			}
		case "user":
			for _, b := range ln.Message.Content {
				if b.Type != "tool_result" {
					continue
				}
				mu.Lock()
				name, diffs := names[b.ToolUseID], proposed[b.ToolUseID]
				mu.Unlock()
				if d := claudeResultDiffs(ln.ToolUseResult); d != nil {
					diffs = d
				}
				r := &agent.NativeToolResult{ToolCallID: b.ToolUseID, Name: name, Diffs: diffs}
				if !topLevel {
					r.ParentToolCallID = *ln.ParentToolUseID
				}
				if text := claudeResultText(b.Content); b.IsError {
					r.Error = text
				} else {
					r.Output = text
				}
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: r})
			}
		case "control_request":
			id, req := ln.RequestID, ln.Request
			go func() { write(p.claudeControl(ctx, nt, id, req)) }()
		case "rate_limit_event":
			p.reportRateLimits(parseClaudeRateLimits(ln.RateLimitInfo))
		case "result":
			l := ln
			res = &l
			_ = proc.stdin.Close()
		}
	}
	_ = proc.stdin.Close()
	waitErr := proc.wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if res == nil {
		msg := "claude exited without a result"
		if tail := proc.stderr(); tail != "" {
			msg += ": " + lastLines(tail, 5)
		} else if waitErr != nil {
			msg += ": " + waitErr.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	input := res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{
		InputTokens:       input,
		OutputTokens:      res.Usage.OutputTokens,
		TotalTokens:       input + res.Usage.OutputTokens,
		CachedInputTokens: res.Usage.CacheReadInputTokens,
		CostUSD:           res.TotalCostUSD,
		CostKnown:         true,
	}})
	if res.IsError {
		return fmt.Errorf("claude: %s", strings.TrimSpace(res.Result))
	}
	return nil
}

// claudeControl answers one control_request: can_use_tool becomes a
// permission_request (or, for AskUserQuestion, ask_user_question rounds).
func (p *CLISubprocessProvider) claudeControl(ctx context.Context, nt *agent.NativeTurn, requestID string, raw json.RawMessage) any {
	var req struct {
		Subtype     string            `json:"subtype"`
		ToolName    string            `json:"tool_name"`
		Input       json.RawMessage   `json:"input"`
		ToolUseID   string            `json:"tool_use_id"`
		Suggestions []json.RawMessage `json:"permission_suggestions"`
		Description string            `json:"description"`
		BlockedPath string            `json:"blocked_path"`
	}
	_ = json.Unmarshal(raw, &req)
	reply := func(body any) any {
		return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": body}}
	}
	if req.Subtype != "can_use_tool" {
		return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": "unsupported control request " + req.Subtype}}
	}
	callID := req.ToolUseID
	if callID == "" {
		callID = requestID
	}
	input := req.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}

	if req.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []struct {
				Question    string `json:"question"`
				MultiSelect bool   `json:"multiSelect"`
				Options     []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(input, &in)
		qs := make([]agent.NativeQuestion, len(in.Questions))
		for i, q := range in.Questions {
			qs[i] = agent.NativeQuestion{ToolCallID: fmt.Sprintf("%s#%d", callID, i), Question: q.Question}
			for _, o := range q.Options {
				qs[i].Options = append(qs[i].Options, o.Label)
			}
		}
		got, err := nt.Questions(ctx, qs)
		if err != nil {
			return reply(map[string]any{"behavior": "deny", "message": "The user didn't answer.", "interrupt": true})
		}
		answers := map[string]any{}
		for i, q := range in.Questions {
			if q.MultiSelect {
				answers[q.Question] = []string{got[i]}
			} else {
				answers[q.Question] = got[i]
			}
		}
		var full map[string]any
		_ = json.Unmarshal(input, &full)
		if full == nil {
			full = map[string]any{}
		}
		full["answers"] = answers
		return reply(map[string]any{"behavior": "allow", "updatedInput": full})
	}

	desc := claudeToolCall(callID, req.ToolName, input).Title
	if req.Description != "" {
		desc = req.Description
	} else if req.BlockedPath != "" {
		desc += " (outside the allowed directories: " + req.BlockedPath + ")"
	}
	var d *diff.FileDiff
	if ds := claudeProposedDiffs(req.ToolName, input); len(ds) > 0 {
		d = ds[0]
	}
	ans, err := nt.Ask(ctx, agent.NativeAsk{ToolCallID: callID, ToolName: req.ToolName, Description: desc, Diff: d})
	if err != nil {
		return reply(map[string]any{"behavior": "deny", "message": "The turn was cancelled.", "interrupt": true})
	}
	if !ans.Allowed {
		msg := "The user denied this tool call."
		if ans.Feedback != "" {
			msg += " They said: " + ans.Feedback
		}
		return reply(map[string]any{"behavior": "deny", "message": msg})
	}
	allow := map[string]any{"behavior": "allow", "updatedInput": input}
	if ans.Remember && len(req.Suggestions) > 0 {
		allow["updatedPermissions"] = req.Suggestions // Claude's own "always allow" rules
	}
	return reply(allow)
}

// claudeToolCall describes one Claude Code tool use for clients.
func claudeToolCall(id, name string, input json.RawMessage) agent.ToolCall {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	tc := agent.ToolCall{ID: id, Name: name, Args: input}
	tc.Native = true
	tc.Title, tc.Kind = name, "other"
	switch name {
	case "Read", "NotebookRead":
		tc.Kind, tc.Title = "read", "Read "+str("file_path")
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		tc.Kind, tc.Title = "edit", name+" "+str("file_path")+str("notebook_path")
	case "Bash":
		tc.Kind, tc.Title = "execute", str("command")
	case "Glob", "Grep":
		tc.Kind, tc.Title = "search", name+" "+str("pattern")
	case "WebFetch":
		tc.Kind, tc.Title = "fetch", "Fetch "+str("url")
	case "WebSearch":
		tc.Kind, tc.Title = "fetch", "Search "+str("query")
	case "Task", "Agent":
		tc.Title = str("description")
	case "TodoWrite", "ExitPlanMode":
		tc.Kind = "think"
	}
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if v := str(k); v != "" {
			tc.Locations = append(tc.Locations, v)
		}
	}
	tc.Title = strings.TrimSpace(tc.Title)
	return tc
}

// claudeProposedDiffs computes what an Edit/MultiEdit/Write would do to the
// file as it is on disk right now — for the permission prompt, and as the
// result's diff when the CLI doesn't report its own patch.
func claudeProposedDiffs(name string, input json.RawMessage) []*diff.FileDiff {
	var in struct {
		FilePath   string `json:"file_path"`
		Content    string `json:"content"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Edits      []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
		return nil
	}
	before := ""
	if b, err := os.ReadFile(in.FilePath); err == nil {
		before = string(b)
	}
	after := before
	replace := func(old, repl string, all bool) {
		if all {
			after = strings.ReplaceAll(after, old, repl)
		} else {
			after = strings.Replace(after, old, repl, 1)
		}
	}
	switch name {
	case "Write":
		after = in.Content
	case "Edit":
		replace(in.OldString, in.NewString, in.ReplaceAll)
	case "MultiEdit":
		for _, e := range in.Edits {
			replace(e.OldString, e.NewString, e.ReplaceAll)
		}
	default:
		return nil
	}
	if before == after {
		return nil
	}
	return []*diff.FileDiff{diff.ComputeDiff(in.FilePath, before, after)}
}

// claudeResultDiffs reads the (undocumented) tool_use_result a file tool's
// result line carries: the file's original content plus Claude's own
// structured patch, when present.
func claudeResultDiffs(raw json.RawMessage) []*diff.FileDiff {
	var r struct {
		FilePath        string `json:"filePath"`
		StructuredPatch []struct {
			OldStart int      `json:"oldStart"`
			OldLines int      `json:"oldLines"`
			NewStart int      `json:"newStart"`
			NewLines int      `json:"newLines"`
			Lines    []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil || r.FilePath == "" || len(r.StructuredPatch) == 0 {
		return nil
	}
	fd := &diff.FileDiff{FilePath: r.FilePath}
	var sb strings.Builder
	for _, h := range r.StructuredPatch {
		hunk := diff.Hunk{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for _, l := range h.Lines {
			if l == "" {
				continue
			}
			hunk.Lines = append(hunk.Lines, diff.DiffLine{Type: diff.LineType(l[:1]), Content: l[1:]})
			sb.WriteString(l + "\n")
		}
		fd.Hunks = append(fd.Hunks, hunk)
	}
	fd.Unified = sb.String()
	return []*diff.FileDiff{fd}
}

// claudeResultText flattens a tool_result's content (a string, or an array
// of {type:"text"} blocks).
func claudeResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseClaudeRateLimits reads a rate_limit_event's rate_limit_info.
// unifiedWindows (5-hour and 7-day, utilization 0..1) is what the CLI
// actually sends; the documented top-level utilization/rateLimitType pair
// is the fallback.
func parseClaudeRateLimits(raw json.RawMessage) *protocol.ProviderRateLimits {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    int64    `json:"resetsAt"`
	}
	var info struct {
		window
		RateLimitType  string            `json:"rateLimitType"`
		UnifiedWindows map[string]window `json:"unifiedWindows"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &info) != nil {
		return nil
	}
	if len(info.UnifiedWindows) == 0 && info.Utilization != nil && info.RateLimitType != "" {
		info.UnifiedWindows = map[string]window{info.RateLimitType: info.window}
	}
	rl := &protocol.ProviderRateLimits{}
	for name, w := range info.UnifiedWindows {
		if w.Utilization == nil {
			continue
		}
		win := &protocol.RateLimitWindow{UsedPct: roundPct(*w.Utilization * 100), ResetsAt: unixRFC3339(w.ResetsAt)}
		switch name {
		case "five_hour":
			rl.FiveHour = win
		case "seven_day":
			rl.SevenDay = win
		case "seven_day_opus", "seven_day_sonnet":
			// Same rows get_usage draws (parseClaudeUsage), so an update mid-
			// turn replaces them rather than adding a second one.
			label := map[string]string{"seven_day_opus": "Opus", "seven_day_sonnet": "Sonnet"}[name]
			rl.Other = append(rl.Other, protocol.OtherRateLimitWindow{WindowMins: 7 * 24 * 60, Label: label, RateLimitWindow: *win})
		}
	}
	return rl
}
