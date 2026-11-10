package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// ClaudeModel drives the Claude Code CLI as a plain tool-calling model, so a Claude Code login can
// be lent to a chat on another server (PLAN_FORWARDING, phase 3). The CLI runs with its own tools
// off and the caller's tools offered over MCP; to the caller each StreamTurn is one round of an
// HTTP model API: it ends with tool calls (the caller runs them and calls again with the results
// appended) or with an answer.
//
// The process is kept per SessionKey. While the caller runs the tools, the CLI's MCP tools/call
// requests are held open here, and the next StreamTurn answers them with the results. A request
// that doesn't continue what the process has seen starts a fresh process seeded with a transcript
// (decide); when in doubt, fresh.
type ClaudeModel struct {
	Model     string
	ConfigDir string
	Env       map[string]string
	// SessionKey names the pooled process. Empty: tool requests are refused, since only a
	// pooled process can hold calls open.
	SessionKey string
	// Start overrides process creation (tests).
	Start procStarter

	effort string
}

func NewClaudeModel(cfg ProviderConfig, model, sessionKey string) *ClaudeModel {
	return &ClaudeModel{Model: model, ConfigDir: cfg.ConfigDir, Env: cfg.Env, SessionKey: sessionKey}
}

func (m *ClaudeModel) ModelName() string { return m.Model }

// SetModelOptions takes the reasoning effort, like the CLI provider does; the rest has no meaning here.
func (m *ClaudeModel) SetModelOptions(o agent.ModelOptions) {
	switch o.ReasoningEffort {
	case "":
	case "default":
		m.effort = ""
	case "ultracode": // a setting on top of xhigh, not a level of its own
		m.effort = "xhigh"
	default:
		m.effort = o.ReasoningEffort
	}
}

const (
	// claudeModelServer is the MCP server's name, so the model sees mcp__y__<tool>.
	claudeModelServer = "y"
	claudeModelPrefix = "mcp__" + claudeModelServer + "__"
	// A held call outlives the CLI's default 60 s MCP timeout (after which the model retries on its own).
	claudeModelToolTimeout = "86400000" // 24 hours, in ms
	claudeModelPrompt      = "\n\nYour tools are named with the prefix " + claudeModelPrefix + ": to call the tool \"x\", use the name " + claudeModelPrefix + "x."
)

// ---- the pool ----

var claudeModels = struct {
	mu   sync.Mutex
	live map[string]*modelSession
}{live: map[string]*modelSession{}}

type action int

const (
	actFresh    action = iota // a new process, seeded with a transcript
	actContinue               // the pending tool calls get their results
	actNext                   // the finished answer gets one more user message
)

// A StreamTurn is one round. The channels close the way the HTTP providers' do: the error, if any,
// is buffered before both are closed.
func (m *ClaudeModel) StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition, systemPrompt string) (<-chan agent.ModelStreamChunk, <-chan error) {
	chunks := make(chan agent.ModelStreamChunk, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		// The process's reader can still emit after the round returned (a cancelled one), so
		// closing and emitting share a lock and emits after the close are dropped.
		var mu sync.Mutex
		closed := false
		defer func() {
			mu.Lock()
			closed = true
			close(chunks)
			mu.Unlock()
		}()
		emit := func(c agent.ModelStreamChunk) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return
			}
			select {
			case chunks <- c:
			case <-ctx.Done():
			}
		}
		var err error
		switch {
		case len(tools) == 0:
			err = m.oneShot(ctx, history, systemPrompt, emit)
		case m.SessionKey == "":
			err = errors.New("a request with tools needs a session to keep the Claude Code process in")
		default:
			err = m.round(ctx, history, tools, systemPrompt, emit)
		}
		if err != nil {
			errs <- err
		}
	}()
	return chunks, errs
}

// oneShot answers a request that offers no tools (a title, a side question, a compaction brief)
// without touching the pooled process.
func (m *ClaudeModel) oneShot(ctx context.Context, history []agent.Message, system string, emit func(agent.ModelStreamChunk)) error {
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: m.Model, ConfigDir: m.ConfigDir, Env: m.Env, Start: m.Start}
	text, usage, err := p.OneShot(ctx, system, claudeFreshPrompt(history))
	if err != nil {
		return err
	}
	usage.CostKnown = true // the CLI's own total, so the borrower doesn't price the tokens at API rates
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text})
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &usage})
	return nil
}

// claudeFreshPrompt is the prompt that seeds a process from history: a transcript of everything
// before the last user message, then that message. Whatever came after it (the tool calls and
// results of a turn whose process was lost) follows as a second transcript, so the work isn't redone.
func claudeFreshPrompt(history []agent.Message) string {
	last := len(history) - 1
	for last > 0 && history[last].Role != "user" {
		last--
	}
	if last < 0 {
		return ""
	}
	p := history[last].Content
	if last > 0 {
		p = "Earlier in this conversation (you don't have this in your context, it's a transcript):\n\n" +
			agent.RenderTranscript(history[:last]) + "\n---\n\n" + p
	}
	if tail := history[last+1:]; len(tail) > 0 {
		p += "\n\n---\n\nSince that message, in this same request:\n\n" + agent.RenderTranscript(tail) + "\nContinue from there."
	}
	return p
}

func (m *ClaudeModel) sig(tools []tool.ToolDefinition, system string) string {
	b, _ := json.Marshal(struct {
		Model, Effort, Dir, System string
		Env                        map[string]string
		Tools                      []tool.ToolDefinition
	}{m.Model, m.effort, m.ConfigDir, system, m.Env, tools})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// round runs one StreamTurn on the pooled process (or a new one).
func (m *ClaudeModel) round(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition, system string, emit func(agent.ModelStreamChunk)) error {
	system += claudeModelPrompt
	s, act, err := m.acquire(ctx, m.sig(tools, system), history, tools, system)
	if err != nil {
		return err
	}
	r := &modelRound{s: s, emit: emit, done: make(chan error, 1)}
	if err := s.begin(r, act, history); err != nil {
		s.close()
		return err
	}
	select {
	case err = <-r.done:
	case <-s.exited:
		// It may have said its piece and then left.
		select {
		case err = <-r.done:
		default:
			err = errors.New("claude exited without finishing the round")
			if tail := s.proc.stderr(); tail != "" {
				err = fmt.Errorf("%w: %s", err, lastLines(tail, 5))
			}
		}
	case <-ctx.Done():
		// Stop what the CLI is doing, fail the held calls and let the process go: whatever state it
		// is in, the next request starts over from the transcript.
		s.write(map[string]any{"type": "control_request", "request_id": "yk-interrupt", "request": map[string]any{"subtype": "interrupt"}})
		s.close()
		return ctx.Err()
	}
	if err != nil {
		s.close()
		return err
	}
	s.end(history)
	return nil
}

// acquire returns the session for this request with its turn taken (one round at a time) and
// what to do on it.
func (m *ClaudeModel) acquire(ctx context.Context, sig string, history []agent.Message, tools []tool.ToolDefinition, system string) (*modelSession, action, error) {
	key := m.SessionKey
	var old *modelSession
	for {
		claudeModels.mu.Lock()
		old = claudeModels.live[key]
		claudeModels.mu.Unlock()
		if old == nil {
			break
		}
		select {
		case old.turn <- struct{}{}:
		case <-old.ended:
			continue
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
		if old.isClosed() {
			claudeModels.mu.Lock()
			if claudeModels.live[key] == old {
				delete(claudeModels.live, key)
			}
			claudeModels.mu.Unlock()
			continue
		}
		break
	}
	if old != nil {
		if act := old.decide(sig, history); act != actFresh {
			return old, act, nil
		}
		old.close() // an edit, a retry, a branch, a changed tool set: not this process's conversation
	}
	s, err := m.launch(sig, tools, system)
	if err != nil {
		return nil, 0, err
	}
	claudeModels.mu.Lock()
	racer := claudeModels.live[key]
	claudeModels.live[key] = s
	claudeModels.mu.Unlock()
	if racer != nil && racer != old { // two rounds of one chat at once: the later wins
		racer.close()
	}
	return s, actFresh, nil
}

// ---- the session ----

type modelSession struct {
	key, sig string
	proc     *nativeProc
	wmu      sync.Mutex
	exited   chan struct{}
	ended    chan struct{} // closed by close
	turn     chan struct{} // holds one token while a round is on it
	srv      *http.Server
	token    string
	list     []any // the tools/list answer
	dir      string
	rmConfig func()

	mu      sync.Mutex
	closed  bool
	failure error
	idle    *time.Timer
	round   *modelRound
	calls   map[string]*heldCall
	seen    []string // fingerprints of the messages the process has consumed
	pending []string // tool-use ids the last round ended with; none after an answer
	cost    float64  // total_cost_usd the process has reported, cumulative
}

// heldCall is one MCP tools/call waiting for its result.
type heldCall struct {
	done chan struct{}
	set  bool // guarded by the session's mu
	res  mcpResult
}

// mcpResult is what a held call is answered with: content, or an error to fail it with.
type mcpResult struct {
	content []any
	err     string
}

func (s *modelSession) write(v any) { _ = writeJSONLine(s.proc.stdin, &s.wmu, v) }

func (s *modelSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *modelSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

func (m *ClaudeModel) launch(sig string, tools []tool.ToolDefinition, system string) (*modelSession, error) {
	s := &modelSession{key: m.SessionKey, sig: sig, exited: make(chan struct{}), ended: make(chan struct{}),
		turn: make(chan struct{}, 1), calls: map[string]*heldCall{}}
	s.turn <- struct{}{}
	for _, t := range tools {
		schema := t.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		s.list = append(s.list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s.token = hex.EncodeToString(tok)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	// A scratch directory of its own, empty, so nothing of the machine's projects rides along.
	if s.dir, err = os.MkdirTemp("", "yoake-claude-model-"); err != nil {
		ln.Close()
		return nil, err
	}
	cfg := claudeMCPConfig([]tool.AgentServer{{Name: claudeModelServer, Type: "http", URL: "http://" + ln.Addr().String() + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + s.token}}})
	cfgPath, rmConfig, err := writeSecretFile("mcp-*.json", []byte(cfg))
	if err != nil {
		ln.Close()
		os.RemoveAll(s.dir)
		return nil, err
	}
	s.rmConfig = rmConfig
	// ponytail: the system prompt is an argument, as the proven command line has it; a prompt over
	// the OS's per-argument limit (128 KiB on Linux) needs --system-prompt-file.
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--tools", "", "--mcp-config", cfgPath, "--strict-mcp-config",
		"--setting-sources", "", "--system-prompt", system, "--system-prompt-snapshot", "off",
		"--permission-prompt-tool", "stdio", "--allowedTools", "mcp__" + claudeModelServer}
	if m.Model != "" {
		args = append(args, "--model", m.Model)
	}
	if m.effort != "" {
		args = append(args, "--effort", m.effort)
	}
	start := startProc
	if m.Start != nil {
		start = m.Start
	}
	env := append(procEnv(CLIKindClaude, m.ConfigDir, m.Env), "MCP_TOOL_TIMEOUT="+claudeModelToolTimeout)
	// The process outlives the round that started it, so it isn't bound to that round's context.
	s.proc, err = envStarter(start, env)(context.Background(), "claude", args, s.dir)
	if err != nil {
		ln.Close()
		rmConfig()
		os.RemoveAll(s.dir)
		return nil, err
	}
	go s.srv.Serve(ln)
	go s.read()
	// Headless Claude Code omits thinking text on recent models unless asked (as startClaude does).
	s.write(map[string]any{"type": "control_request", "request_id": "thinking-display",
		"request": map[string]any{"subtype": "set_max_thinking_tokens", "thinking_display": "summarized"}})
	return s, nil
}

// close ends the session: the held calls fail, the listener and the process go, the scratch
// directory and the MCP config are removed. Safe to call twice.
func (s *modelSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.idle != nil {
		s.idle.Stop()
	}
	for _, c := range s.calls {
		c.resolve(s, mcpResult{err: "the session ended"})
	}
	r := s.round
	s.mu.Unlock()
	close(s.ended)
	if r != nil {
		r.finish(errors.New("the Claude Code session ended"))
	}
	claudeModels.mu.Lock()
	if claudeModels.live[s.key] == s {
		delete(claudeModels.live, s.key)
	}
	claudeModels.mu.Unlock()
	_ = s.srv.Close()
	_ = s.proc.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		if s.proc.kill != nil {
			s.proc.kill()
		}
	}
	_ = os.RemoveAll(s.dir)
	s.rmConfig()
}

// resolve answers a held call once; s.mu is held.
func (c *heldCall) resolve(_ *modelSession, r mcpResult) {
	if !c.set {
		c.res, c.set = r, true
		close(c.done)
	}
}

// fail ends the round in progress with err, or, with none, drops the session: whatever the CLI
// did, its state can't be trusted for another round.
func (s *modelSession) fail(err error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	r := s.round
	s.mu.Unlock()
	if r != nil {
		r.finish(err)
	} else {
		go s.close()
	}
}

// arm starts the idle clock; it takes the turn when it runs out, so a round starting now wins or loses cleanly.
func (s *modelSession) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.idle = time.AfterFunc(idleTimeout(), func() {
		select {
		case s.turn <- struct{}{}:
			s.close()
		default:
		}
	})
}

// decide says what a request does on this session. The rule, in full: the session's recorded
// fingerprints must be a prefix of history, followed by one assistant message (not compared: the
// caller stores its own rendering of what was streamed) and then either exactly the tool results
// for the pending tool-use ids, once each in any order, or, after a finished answer, exactly one
// user message. The tool set, system prompt, model, effort and account must be the ones the
// process was launched with, and the process alive. Anything else is fresh.
func (s *modelSession) decide(sig string, history []agent.Message) action {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.seen)
	if s.closed || s.failure != nil || s.dead() || s.sig != sig || len(history) <= n || history[n].Role != "assistant" ||
		!slices.Equal(fingerprints(history[:n]), s.seen) {
		return actFresh
	}
	rest := history[n+1:]
	if len(s.pending) == 0 {
		if len(history[n].ToolCalls) == 0 && len(rest) == 1 && rest[0].Role == "user" {
			return actNext
		}
		return actFresh
	}
	var called, answered []string
	for _, tc := range history[n].ToolCalls {
		called = append(called, tc.ID)
	}
	for _, m := range rest {
		if m.Role != "tool" {
			return actFresh
		}
		answered = append(answered, m.ToolCallID)
	}
	want := slices.Clone(s.pending)
	sort.Strings(want)
	sort.Strings(called)
	sort.Strings(answered)
	if slices.Equal(called, want) && slices.Equal(answered, want) {
		return actContinue
	}
	return actFresh
}

func fingerprints(h []agent.Message) []string {
	out := make([]string, len(h))
	for i, m := range h {
		ids := make([]string, len(m.ToolCalls))
		for j, tc := range m.ToolCalls {
			ids[j] = tc.ID
		}
		sum := sha256.Sum256([]byte(strings.Join([]string{m.Role, m.Content, strings.Join(ids, ","), m.ToolCallID}, "\x00")))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// begin installs the round and starts it: the results go to the held calls, or a prompt to the process.
func (s *modelSession) begin(r *modelRound, act action, history []agent.Message) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("the Claude Code session ended")
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.round = r
	var results map[string]mcpResult
	switch act {
	case actContinue:
		results = map[string]mcpResult{}
		for _, m := range history[len(s.seen)+1:] {
			results[m.ToolCallID] = toolResult(m)
		}
		for _, id := range s.pending {
			c := s.calls[id]
			if c == nil { // its request hasn't come yet: it will find the result waiting
				c = &heldCall{done: make(chan struct{})}
				s.calls[id] = c
			}
			c.resolve(s, results[id])
		}
		s.pending = nil
	case actNext:
		s.pending = nil
	default:
		s.seen, s.pending = nil, nil
	}
	s.mu.Unlock()
	switch act {
	case actNext:
		s.write(claudeUserLine(history[len(history)-1].Content))
	case actFresh:
		s.write(claudeUserLine(claudeFreshPrompt(history)))
	}
	return nil
}

// end records what the process has now consumed and lets the next round in.
func (s *modelSession) end(history []agent.Message) {
	s.mu.Lock()
	s.seen = fingerprints(history)
	s.mu.Unlock()
	<-s.turn
	s.arm()
}

// toolResult is a tool message as MCP content: its text, and its pictures where MCP can take them.
func toolResult(m agent.Message) mcpResult {
	content := []any{map[string]any{"type": "text", "text": m.Content}}
	for _, img := range m.Images {
		if strings.HasPrefix(img.MimeType, "image/") && img.Data != "" && !strings.HasPrefix(img.Data, "data:") {
			content = append(content, map[string]any{"type": "image", "data": img.Data, "mimeType": img.MimeType})
		} else {
			content = append(content, map[string]any{"type": "text", "text": "[an image was omitted]"})
		}
	}
	return mcpResult{content: content}
}

// ---- the process's output ----

func (s *modelSession) read() {
	for s.proc.lines.Scan() {
		var ln claudeLine
		if json.Unmarshal(s.proc.lines.Bytes(), &ln) != nil {
			continue
		}
		if ln.Type == "control_request" {
			go s.control(ln)
			continue
		}
		s.mu.Lock()
		r := s.round
		s.mu.Unlock()
		if r != nil { // between rounds nothing is listening, and nothing should be said
			r.handle(ln)
		}
	}
	close(s.exited)
	_ = s.proc.wait()
	s.close()
}

// control answers the CLI's permission asks: the offered tools are allowed (the caller does the
// real permission checks), anything else is denied and ends the round, since the CLI is using
// something that was supposed to be off.
func (s *modelSession) control(ln claudeLine) {
	var req struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(ln.Request, &req)
	resp := func(body map[string]any) {
		body["request_id"] = ln.RequestID
		s.write(map[string]any{"type": "control_response", "response": body})
	}
	switch {
	case req.Subtype != "can_use_tool":
		resp(map[string]any{"subtype": "error", "error": "unsupported control request " + req.Subtype})
	case strings.HasPrefix(req.ToolName, claudeModelPrefix):
		input := req.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		resp(map[string]any{"subtype": "success", "response": map[string]any{"behavior": "allow", "updatedInput": input}})
	default:
		resp(map[string]any{"subtype": "success", "response": map[string]any{"behavior": "deny", "message": "This tool isn't available."}})
		s.fail(fmt.Errorf("claude tried to use %q, which isn't one of the offered tools", req.ToolName))
	}
}

// modelRound is what one StreamTurn has seen so far of the process's output.
type modelRound struct {
	s    *modelSession
	emit func(agent.ModelStreamChunk)
	done chan error // the round's end, once

	// reader goroutine only
	uses  []claudeBlock // tool_use blocks of the message in progress
	msg   claudeUsage   // its latest counts
	total claudeUsage   // the round's, over every message
	last  int           // context size after the last message
	over  bool
}

// finish ends the round, once; nil is success.
func (r *modelRound) finish(err error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.over {
		return
	}
	r.over = true
	if r.s.round == r {
		r.s.round = nil
	}
	r.done <- err
}

func (r *modelRound) handle(ln claudeLine) {
	topLevel := ln.ParentToolUseID == nil || *ln.ParentToolUseID == ""
	switch ln.Type {
	case "system":
		if ln.Subtype != "init" {
			return
		}
		status, hasTools := "missing", false
		for _, sv := range ln.MCPServers {
			if sv.Name == claudeModelServer {
				status = sv.Status
			}
		}
		for _, t := range ln.Tools {
			hasTools = hasTools || strings.HasPrefix(t, claudeModelPrefix)
		}
		// Without its server the CLI goes on with no tools and the model invents calls as text.
		if status != "connected" || !hasTools {
			r.s.fail(fmt.Errorf("claude couldn't use the offered tools (tool server %s, tools offered: %v)", status, hasTools))
		}
	case "stream_event":
		if !topLevel {
			return
		}
		switch ln.Event.Type {
		case "content_block_delta":
			switch ln.Event.Delta.Type {
			case "text_delta":
				r.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: ln.Event.Delta.Text})
			case "thinking_delta":
				r.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: ln.Event.Delta.Thinking})
			}
		case "message_delta":
			r.msg = ln.Event.Usage
		case "message_stop":
			r.messageStop()
		}
	case "assistant":
		if !topLevel {
			return
		}
		// One line per content block: the tool calls are collected here, and the round ends at message_stop.
		if u := ln.Message.Usage; u != (claudeUsage{}) {
			r.msg = u
		}
		for _, b := range ln.Message.Content {
			if b.Type == "tool_use" && !slices.ContainsFunc(r.uses, func(x claudeBlock) bool { return x.ID == b.ID }) {
				r.uses = append(r.uses, b)
			}
		}
	case "result":
		r.result(ln)
	}
}

// messageStop closes a message's accounting and, when it called tools, ends the round with them.
func (r *modelRound) messageStop() {
	u := r.msg
	r.msg = claudeUsage{}
	r.total.InputTokens += u.InputTokens
	r.total.CacheCreationInputTokens += u.CacheCreationInputTokens
	r.total.CacheReadInputTokens += u.CacheReadInputTokens
	r.total.OutputTokens += u.OutputTokens
	r.last = claudeContextTokens(u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, u.OutputTokens)
	if len(r.uses) == 0 {
		return // an answer: the result line follows
	}
	ids := make([]string, len(r.uses))
	calls := make([]agent.ToolCall, len(r.uses))
	for i, b := range r.uses {
		name, ok := strings.CutPrefix(b.Name, claudeModelPrefix)
		if !ok {
			r.s.fail(fmt.Errorf("claude called %q, which isn't one of the offered tools", b.Name))
			return
		}
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		ids[i], calls[i] = b.ID, agent.ToolCall{ID: b.ID, Name: name, Args: input}
	}
	for i := range calls {
		r.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &calls[i]})
	}
	r.emitUsage(0, 0)
	s := r.s
	s.mu.Lock()
	s.dropAnswered()
	for _, id := range ids {
		if s.calls[id] == nil {
			s.calls[id] = &heldCall{done: make(chan struct{})}
		}
	}
	// A call that came in for an id this message doesn't have can't ever be answered.
	for id, c := range s.calls {
		if !slices.Contains(ids, id) {
			c.resolve(s, mcpResult{err: "unknown tool call " + id})
		}
	}
	s.pending = ids
	s.mu.Unlock()
	r.finish(nil)
}

// dropAnswered forgets the calls that have their results; s.mu is held.
func (s *modelSession) dropAnswered() {
	for id, c := range s.calls {
		if c.set {
			delete(s.calls, id)
		}
	}
}

func (r *modelRound) result(ln claudeLine) {
	s := r.s
	s.mu.Lock()
	// total_cost_usd is the process's running total; the borrower is told this round's share
	// (every round since the last result, which is the whole turn).
	cost := ln.TotalCostUSD - s.cost
	if cost < 0 {
		cost = ln.TotalCostUSD
	}
	s.cost = ln.TotalCostUSD
	s.pending = nil
	s.dropAnswered()
	s.mu.Unlock()
	window := 0
	for _, mu := range ln.ModelUsage {
		window = max(window, mu.ContextWindow)
	}
	r.emitUsage(cost, window)
	if ln.IsError {
		r.finish(fmt.Errorf("claude: %s", strings.TrimSpace(ln.Result)))
		return
	}
	r.finish(nil)
}

// emitUsage reports the round's tokens (summed over its messages, not the result's turn total,
// which would count earlier rounds again) and the size of the context after the last message.
// CostKnown is always set: the cost is the CLI's own figure, and the borrower must not price the
// tokens itself at API rates.
func (r *modelRound) emitUsage(cost float64, window int) {
	t := r.total
	in := t.InputTokens + t.CacheCreationInputTokens + t.CacheReadInputTokens
	if r.last > 0 {
		r.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkContext, Context: &agent.ContextSnapshot{ContextTokens: r.last, WindowTokens: window, Source: "provider"}})
	}
	r.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{
		InputTokens: in, OutputTokens: t.OutputTokens, TotalTokens: in + t.OutputTokens,
		CachedInputTokens: t.CacheReadInputTokens, CacheWriteInputTokens: t.CacheCreationInputTokens,
		ContextTokens: r.last, WindowTokens: window, CostUSD: cost, CostKnown: true,
	}})
}

// ---- the MCP endpoint ----

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ServeHTTP is a minimal MCP server over POST (like cmd/yoakeland's browser server): the tools
// are the request's, and a tools/call runs nothing, it waits for the result a later round supplies.
func (s *modelSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	var req mcpRequest
	if err != nil || json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any, code int, msg string) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if code != 0 {
			out["error"] = map[string]any{"code": code, "message": msg}
		} else {
			out["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-03-26"
		}
		reply(map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": claudeModelServer, "version": "1"}}, 0, "")
	case "ping":
		reply(map[string]any{}, 0, "")
	case "tools/list":
		reply(map[string]any{"tools": s.list}, 0, "")
	case "tools/call":
		var p struct {
			Meta map[string]any `json:"_meta"`
		}
		_ = json.Unmarshal(req.Params, &p)
		id, _ := p.Meta["claudecode/toolUseId"].(string)
		if res := s.hold(r.Context(), id); res.err != "" {
			reply(nil, -32000, res.err)
		} else {
			reply(map[string]any{"content": res.content}, 0, "")
		}
	default:
		reply(nil, -32601, "method not found")
	}
}

// hold waits for the result of tool-use id. A call can arrive before the message that holds its
// block has ended, so while a round is running an unseen id is taken in; the round's end fails it
// if the message turns out not to have it.
func (s *modelSession) hold(ctx context.Context, id string) mcpResult {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return mcpResult{err: "the session ended"}
	}
	c := s.calls[id]
	if c == nil {
		if id == "" || s.round == nil {
			s.mu.Unlock()
			return mcpResult{err: "unknown tool call " + id}
		}
		c = &heldCall{done: make(chan struct{})}
		s.calls[id] = c
	}
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.res
	case <-ctx.Done():
		// The CLI gave up on the call (its timeout, or an interrupt): what it does next is
		// unknown, and nothing could answer it any more.
		if !s.isClosed() {
			s.fail(errors.New("claude stopped waiting for a tool result"))
		}
		return mcpResult{err: "the call was abandoned"}
	}
}
