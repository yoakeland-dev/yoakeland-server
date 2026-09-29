package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
// undocumented piece) can be answered on it. The process outlives the turn
// (claudeSession) when the turn has a SessionKey; without one stdin is closed
// once `result` arrives, which ends it.

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
	case agent.ModeAcceptEdits:
		return "acceptEdits"
	case agent.ModeAuto:
		return "auto"
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
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
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
		Content    []claudeBlock `json:"content"`
		StopReason string        `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Error         string          `json:"error"`
	ToolUseResult json.RawMessage `json:"tool_use_result"`
	RequestID     string          `json:"request_id"`
	Request       json.RawMessage `json:"request"`
	Response      json.RawMessage `json:"response"`
	UUID          string          `json:"uuid"`
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

// claudeSession is one live `claude` process. Its reader hands each line to
// the turn in progress and drops what arrives between turns.
type claudeSession struct {
	proc   *nativeProc
	wmu    sync.Mutex
	exited chan struct{}

	mu       sync.Mutex
	turn     *claudeTurn
	id       string  // Claude's session id
	model    string  // the model the process is on
	permMode string  // and its permission mode
	cost     float64 // total_cost_usd it has reported, which is cumulative over the process
	nextReq  int
	waiting  map[string]chan claudeLine // control requests we sent, by request_id
}

func (s *claudeSession) write(v any) { _ = writeJSONLine(s.proc.stdin, &s.wmu, v) }

func (s *claudeSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// close asks the process to leave by closing its stdin, and kills it if it stays.
func (s *claudeSession) close() {
	_ = s.proc.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		if s.proc.kill != nil {
			s.proc.kill()
		}
	}
}

func (s *claudeSession) setTurn(t *claudeTurn) {
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
}

func (s *claudeSession) sessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// request sends a control_request and waits for its control_response.
func (s *claudeSession) request(ctx context.Context, subtype string, extra map[string]any) (claudeLine, error) {
	ch := make(chan claudeLine, 1)
	s.mu.Lock()
	s.nextReq++
	id := fmt.Sprintf("yk-%d", s.nextReq)
	s.waiting[id] = ch
	s.mu.Unlock()
	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	s.write(map[string]any{"type": "control_request", "request_id": id, "request": req})
	select {
	case r := <-ch:
		return r, nil
	case <-s.exited:
		return claudeLine{}, fmt.Errorf("claude exited before answering %s", subtype)
	case <-ctx.Done():
		return claudeLine{}, ctx.Err()
	}
}

func (s *claudeSession) read() {
	for s.proc.lines.Scan() {
		var ln claudeLine
		if json.Unmarshal(s.proc.lines.Bytes(), &ln) != nil {
			continue
		}
		s.mu.Lock()
		if ln.SessionID != "" && s.id == "" {
			s.id = ln.SessionID
		}
		t := s.turn
		var waiter chan claudeLine
		if ln.Type == "control_response" {
			var r struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(ln.Response, &r)
			waiter = s.waiting[r.RequestID]
			delete(s.waiting, r.RequestID)
		}
		s.mu.Unlock()
		switch {
		case waiter != nil:
			waiter <- ln
		case t != nil:
			t.handle(ln)
		case ln.Type == "control_request":
			// Nobody is here to ask (a background task outliving its turn): deny rather than leave it waiting.
			s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": ln.RequestID, "error": "no turn is running"}})
		}
	}
	// No more output: as far as anyone is concerned it is gone. Reap it after.
	close(s.exited)
	_ = s.proc.wait()
}

// claudeTurn is what one turn has seen so far of the session's output.
type claudeTurn struct {
	p    *CLISubprocessProvider
	s    *claudeSession
	ctx  context.Context
	nt   *agent.NativeTurn
	emit func(agent.ModelStreamChunk)
	done chan claudeLine // the result line
	seen atomic.Bool     // the session has said something since the prompt went in
	over atomic.Bool     // the turn has ended, so there is nothing left to steer

	lastAssistant atomic.Value // uuid of the last assistant message: where this turn ends, to fork at later

	mu       sync.Mutex
	names    map[string]string           // tool_use_id -> tool name
	proposed map[string][]*diff.FileDiff // tool_use_id -> diff computed before it ran
}

func (t *claudeTurn) handle(ln claudeLine) {
	t.seen.Store(true)
	nt, emit := t.nt, t.emit
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
		if topLevel && ln.UUID != "" {
			t.lastAssistant.Store(ln.UUID)
		}
		// Text already streamed as deltas; only the tool calls are new.
		// A subagent's (Task) own calls carry the Task's id as parent.
		for _, b := range ln.Message.Content {
			if b.Type != "tool_use" {
				continue
			}
			t.mu.Lock()
			t.names[b.ID] = b.Name
			t.proposed[b.ID] = claudeProposedDiffs(b.Name, b.Input)
			t.mu.Unlock()
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
			t.mu.Lock()
			name, diffs := t.names[b.ToolUseID], t.proposed[b.ToolUseID]
			t.mu.Unlock()
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
		go func() { t.s.write(t.p.claudeControl(t.ctx, nt, id, req)) }()
	case "rate_limit_event":
		t.p.reportRateLimits(parseClaudeRateLimits(ln.RateLimitInfo))
	case "result":
		t.over.Store(true)
		select {
		case t.done <- ln:
		default:
		}
	}
}

// claudeLaunch is what the process is started with. Everything here but the
// model and the permission mode is a launch argument, so a change to it
// restarts the process (resuming the chat); those two are switched in session.
type claudeLaunch struct {
	effort   string
	settings string // --settings JSON, "" for none
	permMode string
}

func (p *CLISubprocessProvider) claudeLaunch(nt *agent.NativeTurn) claudeLaunch {
	// Ultracode is a Claude Code setting on top of xhigh, not an effort level of its own.
	effort, settings := p.effort, map[string]any{}
	if effort == "ultracode" {
		effort, settings["ultracode"] = "xhigh", true
	}
	if p.fast {
		settings["fastMode"] = true
	}
	if nt.Mode == agent.ModeAutonomous {
		_ = json.Unmarshal([]byte(claudeAutonomousSandbox), &settings)
	}
	l := claudeLaunch{effort: effort, permMode: claudePermissionMode(nt.Mode)}
	if len(settings) > 0 {
		raw, _ := json.Marshal(settings)
		l.settings = string(raw)
	}
	return l
}

func (p *CLISubprocessProvider) startClaude(nt *agent.NativeTurn, l claudeLaunch) (*claudeSession, error) {
	// A model without auto mode takes --permission-mode auto at launch and runs
	// with no reviewer; only set_permission_mode refuses it. So auto is never a
	// launch argument: the session starts in default and is switched, which fails
	// where auto isn't available.
	launchMode := l.permMode
	if launchMode == "auto" {
		launchMode = "default"
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", launchMode}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	if l.effort != "" {
		args = append(args, "--effort", l.effort)
	}
	if nt.ResumeID != "" {
		args = append(args, "--resume", nt.ResumeID)
		if nt.ForkAt != "" {
			// A new session holding this one as it was at that message. --resume-session-at
			// isn't in --help but truncates the fork there (checked against the CLI).
			args = append(args, "--fork-session", "--resume-session-at", nt.ForkAt)
		}
	}
	if l.settings != "" {
		args = append(args, "--settings", l.settings)
	}
	// The process outlives the turn that started it, so it isn't bound to the turn's context.
	proc, err := p.start()(context.Background(), "claude", args, p.Dir)
	if err != nil {
		return nil, err
	}
	id := nt.ResumeID
	if nt.ForkAt != "" {
		id = "" // the fork has an id of its own, known when it says so
	}
	s := &claudeSession{proc: proc, exited: make(chan struct{}), id: id, model: p.Model, permMode: launchMode, waiting: map[string]chan claudeLine{}}
	go s.read()
	// Headless Claude Code omits thinking text on Sonnet/Opus 4.7+ unless asked;
	// a CLI that predates the request just answers with an error we ignore.
	s.write(map[string]any{
		"type": "control_request", "request_id": "thinking-display",
		"request": map[string]any{"subtype": "set_max_thinking_tokens", "thinking_display": "summarized"},
	})
	return s, nil
}

// errStaleSession: a process kept from an earlier turn had died before it
// answered anything, so the turn can be sent again to a new one.
var errStaleSession = errors.New("the agent process had already exited")

// errForkFailed: the session couldn't be forked at the checkpoint, before it said anything.
var errForkFailed = errors.New("the agent's session couldn't be forked there")

func (p *CLISubprocessProvider) runClaude(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	err := p.runClaudeOnce(ctx, nt, emit)
	if errors.Is(err, errStaleSession) {
		err = p.runClaudeOnce(ctx, nt, emit)
	}
	if errors.Is(err, errForkFailed) {
		// Start over from the transcript rather than failing the turn.
		nt.ResumeID, nt.ForkAt, nt.Prompt = "", "", nt.FreshPrompt
		err = p.runClaudeOnce(ctx, nt, emit)
	}
	return err
}

func (p *CLISubprocessProvider) runClaudeOnce(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	l := p.claudeLaunch(nt)
	sig := strings.Join([]string{p.Dir, l.effort, l.settings}, "\x00")
	usable := func(ns nativeSession) bool {
		cs := ns.(*claudeSession)
		cs.mu.Lock()
		defer cs.mu.Unlock()
		// A live process can't start a different conversation, fork one, or fall back to the default model.
		return nt.ForkAt == "" && cs.id == nt.ResumeID && (p.Model != "" || cs.model == "")
	}
	ns, reused, release, err := acquireSession(nt, CLIKindClaude, sig, usable, func() (nativeSession, error) { return p.startClaude(nt, l) })
	if err != nil {
		return err
	}
	defer release()
	s := ns.(*claudeSession)

	t := &claudeTurn{p: p, s: s, ctx: ctx, nt: nt, emit: emit, done: make(chan claudeLine, 1),
		names: map[string]string{}, proposed: map[string][]*diff.FileDiff{}}
	s.setTurn(t)
	defer s.setTurn(nil)

	s.mu.Lock()
	changeModel, changeMode := p.Model != s.model, l.permMode != s.permMode
	s.mu.Unlock()
	if changeModel {
		if r, err := s.request(ctx, "set_model", map[string]any{"model": p.Model}); err != nil {
			return err
		} else if e := controlError(r); e != "" {
			return fmt.Errorf("claude: %s", e)
		}
		s.mu.Lock()
		s.model = p.Model
		s.mu.Unlock()
	}
	if changeMode {
		if r, err := s.request(ctx, "set_permission_mode", map[string]any{"mode": l.permMode}); err != nil {
			return err
		} else if e := controlError(r); e != "" {
			return fmt.Errorf("claude: %s", e)
		}
		s.mu.Lock()
		s.permMode = l.permMode
		s.mu.Unlock()
	}

	userLine := func(text string) map[string]any {
		return map[string]any{
			"type":               "user",
			"message":            map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
			"parent_tool_use_id": nil,
		}
	}
	s.write(userLine(nt.Prompt))
	// A plain user line while the turn runs is folded into it at the agent's next
	// model call, and one result still ends the whole exchange. (priority "now"
	// would interrupt instead.)
	defer t.over.Store(true)
	nt.SetSteer(func(text string) error {
		if t.over.Load() || s.dead() {
			return errors.New("the turn has ended")
		}
		s.write(userLine(text))
		return nil
	})

	var res claudeLine
	select {
	case res = <-t.done:
	case <-s.exited:
		// It may have said its piece and then left, which leaves both ready.
		select {
		case res = <-t.done:
		default:
			if reused && !t.seen.Load() {
				return errStaleSession
			}
			if nt.ForkAt != "" && !t.seen.Load() {
				return errForkFailed
			}
			msg := "claude exited without a result"
			if tail := s.proc.stderr(); tail != "" {
				msg += ": " + lastLines(tail, 5)
			}
			return fmt.Errorf("%s", msg)
		}
	case <-ctx.Done():
		// Stop the turn, not the process: an interrupt ends it with a result and
		// the session stays usable. A process that ignores it is killed.
		s.write(map[string]any{"type": "control_request", "request_id": "yk-interrupt", "request": map[string]any{"subtype": "interrupt"}})
		select {
		case <-t.done:
		case <-s.exited:
		case <-time.After(5 * time.Second):
			s.close()
		}
		return ctx.Err()
	}

	// total_cost_usd is the process's running total; usage is this turn's.
	if u, ok := t.lastAssistant.Load().(string); ok {
		nt.Checkpoint = u
	}
	s.mu.Lock()
	cost := res.TotalCostUSD - s.cost
	if cost < 0 {
		cost = res.TotalCostUSD
	}
	s.cost = res.TotalCostUSD
	s.mu.Unlock()
	input := res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{
		InputTokens:       input,
		OutputTokens:      res.Usage.OutputTokens,
		TotalTokens:       input + res.Usage.OutputTokens,
		CachedInputTokens: res.Usage.CacheReadInputTokens,
		CostUSD:           cost,
		CostKnown:         true,
	}})
	if res.IsError {
		return fmt.Errorf("claude: %s", strings.TrimSpace(res.Result))
	}
	return nil
}

// controlError is the message of a control_response that failed, else "".
func controlError(ln claudeLine) string {
	var r struct {
		Subtype string `json:"subtype"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(ln.Response, &r) == nil && r.Subtype == "error" {
		return r.Error
	}
	return ""
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
	ask := agent.NativeAsk{ToolCallID: callID, ToolName: req.ToolName, Description: desc, Diff: d}
	switch req.ToolName {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		ask.Edit = true
		// Plan mode's own scratch file: Claude writes its plan here before
		// asking to leave plan mode. Not a change to the project.
		if nt.Mode == agent.ModePlan && isClaudePlanFile(input) {
			return reply(map[string]any{"behavior": "allow", "updatedInput": input})
		}
	case "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(input, &in)
		ask.Plan = in.Plan
	}
	ans, err := nt.Ask(ctx, ask)
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

// isClaudePlanFile: the write targets a markdown file directly in
// ~/.claude/plans, where Claude keeps its plans.
func isClaudePlanFile(input json.RawMessage) bool {
	var in struct {
		FilePath string `json:"file_path"`
	}
	home, err := os.UserHomeDir()
	if json.Unmarshal(input, &in) != nil || err != nil {
		return false
	}
	dir := filepath.Join(home, ".claude", "plans")
	p := filepath.Clean(in.FilePath)
	return filepath.Dir(p) == dir && filepath.Ext(p) == ".md"
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
