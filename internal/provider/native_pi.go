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
)

// Pi and Oh My Pi (a fork of it), driven through their RPC mode (SPEC.md §6b):
//
//	pi  --mode rpc --approve      (with PI_SKIP_VERSION_CHECK=1)
//	omp --mode rpc --yolo
//
// Newline-delimited JSON, but request/response rather than JSON-RPC: each request
// carries a string id and is answered by {"type":"response","id","success","data"};
// everything else on the stream is an event. A prompt's response only says it was
// accepted; the turn ends on an event (agent_settled for Pi, a terminal agent_end
// for Oh My Pi). Pi has no permission system and Oh My Pi's is bypassed by --yolo,
// so both run with full access only, and a chat in any other mode is refused.
//
// Not yet run against the real CLIs (none installed here): this follows what
// Waku observed. Oh My Pi's chunked frames (protocol v2) aren't negotiated, so a
// tool result over its 1 MiB frame limit arrives as an error frame.

type piFlavor struct {
	name       string
	binary     string
	fullAccess string   // the flag that turns permission checks off
	settled    string   // the event that ends a run
	env        []string // set through env(1), since processes start without an environment argument
	probeArgs  []string // a cheap process to ask for models
}

var (
	piFlavorPi = piFlavor{
		name: "Pi", binary: "pi", fullAccess: "--approve", settled: "agent_settled",
		env:       []string{"PI_SKIP_VERSION_CHECK=1"},
		probeArgs: []string{"--mode", "rpc", "--no-session", "--no-skills", "--no-prompt-templates", "--no-context-files"},
	}
	// Oh My Pi rejects a flag it doesn't know, and spells context files --no-rules.
	piFlavorOhMyPi = piFlavor{
		name: "Oh My Pi", binary: "omp", fullAccess: "--yolo", settled: "agent_end",
		probeArgs: []string{"--mode", "rpc", "--no-session", "--no-skills", "--no-rules", "--no-extensions"},
	}
)

func piFlavorOf(k CLIKind) piFlavor {
	if k == CLIKindOhMyPi {
		return piFlavorOhMyPi
	}
	return piFlavorPi
}

// command is what to start: the flavor's binary, behind env(1) when it needs variables set.
func (f piFlavor) command(args ...string) (string, []string) {
	if len(f.env) == 0 {
		return f.binary, args
	}
	return "env", append(append(append([]string(nil), f.env...), f.binary), args...)
}

// piModes: only full access, for the reasons above.
func piModes(f piFlavor) []ModeSpec {
	return []ModeSpec{{Mode: string(agent.ModeAutonomous), Policy: polAutonomous, EnforcedBy: "agent", Native: f.fullAccess}}
}

const (
	piInitTimeout = 30 * time.Second // the agent doesn't answer until it has loaded its extensions and models
	piRPCTimeout  = 10 * time.Second
)

// piLine is one line of the stream: a response or an event.
type piLine struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`

	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	} `json:"message"`
	Update struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`

	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Args          json.RawMessage `json:"args"`
	PartialResult json.RawMessage `json:"partialResult"`
	Result        json.RawMessage `json:"result"`
	IsError       bool            `json:"isError"`

	IsTerminal   *bool  `json:"isTerminal"`
	FinalError   string `json:"finalError"`
	ErrorMessage string `json:"errorMessage"`
	Reason       string `json:"reason"`
	Method       string `json:"method"`
}

func (l piLine) failure(flavor piFlavor) string {
	for _, s := range []string{l.Error, l.ErrorMessage, l.Reason} {
		if s != "" {
			return s
		}
	}
	return flavor.name + " reported an error"
}

type piSession struct {
	proc   *nativeProc
	flavor piFlavor
	wmu    sync.Mutex
	exited chan struct{}

	mu       sync.Mutex
	turn     *piTurn
	nextReq  int
	pending  map[string]chan piLine // requests waiting for their response
	promptID string                 // the prompt request whose turn is running
	file     string                 // the session file, which is what resuming needs (else its id)
	model    string
	effort   string
}

func (s *piSession) write(v any) error { return writeJSONLine(s.proc.stdin, &s.wmu, v) }

func (s *piSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

func (s *piSession) close() {
	_ = s.proc.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		if s.proc.kill != nil {
			s.proc.kill()
		}
	}
}

func (s *piSession) setTurn(t *piTurn) {
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
}

func (s *piSession) newID() string {
	s.nextReq++
	return fmt.Sprintf("yk-%d", s.nextReq)
}

// request sends a command and waits for its response.
func (s *piSession) request(ctx context.Context, timeout time.Duration, req map[string]any) (piLine, error) {
	ch := make(chan piLine, 1)
	s.mu.Lock()
	id := s.newID()
	s.pending[id] = ch
	s.mu.Unlock()
	req["id"] = id
	if err := s.write(req); err != nil {
		return piLine{}, err
	}
	select {
	case r := <-ch:
		if r.Success == nil || !*r.Success {
			if r.Error == "" {
				r.Error = s.flavor.name + " RPC command failed"
			}
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-s.exited:
		return piLine{}, fmt.Errorf("%s exited before answering %v", s.flavor.name, req["type"])
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return piLine{}, fmt.Errorf("%v timed out after %s", req["type"], timeout)
	case <-ctx.Done():
		return piLine{}, ctx.Err()
	}
}

func (s *piSession) read() {
	for s.proc.lines.Scan() {
		var ln piLine
		if json.Unmarshal(s.proc.lines.Bytes(), &ln) != nil {
			continue
		}
		s.mu.Lock()
		t := s.turn
		isPrompt := ln.Type == "response" && ln.ID != "" && ln.ID == s.promptID
		var waiter chan piLine
		if ln.Type == "response" && !isPrompt {
			waiter = s.pending[ln.ID]
			delete(s.pending, ln.ID)
		}
		s.mu.Unlock()
		switch {
		case isPrompt:
			if t != nil {
				t.promptResponse(ln)
			}
		case waiter != nil:
			waiter <- ln
		case ln.Type == "extension_ui_request":
			// An extension asking the user something: there is nobody to ask, so it is cancelled.
			if ln.Method == "select" || ln.Method == "confirm" || ln.Method == "input" || ln.Method == "editor" {
				// Not from the reader: a write that blocks would stop it reading, and the agent may be
				// blocked writing to it.
				go func(id string) {
					_ = s.write(map[string]any{"type": "extension_ui_response", "id": id, "cancelled": true})
				}(ln.ID)
			}
		case t != nil:
			t.handle(ln)
		}
	}
	close(s.exited)
	_ = s.proc.wait()
}

// piTurn is what one turn has seen of the stream.
type piTurn struct {
	s    *piSession
	nt   *agent.NativeTurn
	emit func(agent.ModelStreamChunk)
	done chan struct{} // the turn ended
	over atomic.Bool
	seen atomic.Bool

	mu           sync.Mutex
	started      bool
	sawText      bool // this message streamed text, so message_end needn't repeat it
	sawReasoning bool
	failed       bool              // the run reported a failure it didn't recover from
	err          string            // the latest error the agent reported
	refused      string            // why the prompt itself was refused
	tools        map[string]string // toolCallId -> tool name
}

func (t *piTurn) finish(refused string) {
	t.mu.Lock()
	t.refused = refused
	t.mu.Unlock()
	if !t.over.Swap(true) {
		close(t.done)
	}
}

// promptResponse: acceptance says nothing about the turn, which runs until it
// settles; a refusal, or an extension that handled the prompt itself, ends it.
func (t *piTurn) promptResponse(ln piLine) {
	t.seen.Store(true)
	ok := ln.Success != nil && *ln.Success
	var d struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	_ = json.Unmarshal(ln.Data, &d)
	if ok && (d.AgentInvoked == nil || *d.AgentInvoked) {
		return
	}
	t.s.mu.Lock()
	t.s.promptID = ""
	t.s.mu.Unlock()
	msg := ""
	if !ok {
		msg = ln.Error
		if msg == "" {
			msg = t.s.flavor.name + " RPC command failed"
		}
	}
	t.finish(msg)
}

func (t *piTurn) handle(ln piLine) {
	t.seen.Store(true)
	switch ln.Type {
	case t.s.flavor.settled:
		// Oh My Pi reuses agent_end for intermediate settles and marks the real one.
		if ln.IsTerminal != nil && !*ln.IsTerminal {
			return
		}
		t.mu.Lock()
		started := t.started
		t.mu.Unlock()
		if !started {
			return
		}
		t.s.mu.Lock()
		t.s.promptID = ""
		t.s.mu.Unlock()
		t.finish("")
	case "agent_start", "turn_start":
		t.mu.Lock()
		if !t.started {
			t.started, t.failed = true, false
		}
		t.mu.Unlock()
	case "message_start":
		if ln.Message.Role == "assistant" {
			t.mu.Lock()
			t.sawText, t.sawReasoning = false, false
			t.mu.Unlock()
		}
	case "message_update":
		switch ln.Update.Type {
		case "text_delta":
			if ln.Update.Delta != "" {
				t.mu.Lock()
				t.sawText = true
				t.mu.Unlock()
				t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: ln.Update.Delta})
			}
		case "thinking_delta":
			if ln.Update.Delta != "" {
				t.mu.Lock()
				t.sawReasoning = true
				t.mu.Unlock()
				t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: ln.Update.Delta})
			}
		case "error":
			t.mu.Lock()
			t.failed, t.err = true, ln.failure(t.s.flavor)
			t.mu.Unlock()
		}
	case "message_end":
		if ln.Message.Role != "assistant" {
			return
		}
		// A message that streamed nothing (a provider without deltas) still has its content.
		t.mu.Lock()
		sawText, sawReasoning := t.sawText, t.sawReasoning
		t.sawText, t.sawReasoning = true, true
		t.mu.Unlock()
		for _, b := range ln.Message.Content {
			if b.Type == "text" && !sawText && b.Text != "" {
				t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: b.Text})
			}
			if b.Type == "thinking" && !sawReasoning && b.Thinking != "" {
				t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: b.Thinking})
			}
		}
	case "tool_execution_start":
		t.mu.Lock()
		t.tools[ln.ToolCallID] = ln.ToolName
		t.mu.Unlock()
		tc := claudeToolCall(ln.ToolCallID, ln.ToolName, ln.Args)
		t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
	case "tool_execution_end":
		t.mu.Lock()
		name := t.tools[ln.ToolCallID]
		delete(t.tools, ln.ToolCallID)
		t.mu.Unlock()
		if name == "" {
			name = ln.ToolName
		}
		r := &agent.NativeToolResult{ToolCallID: ln.ToolCallID, Name: name}
		if text := piResultText(ln.Result); ln.IsError {
			r.Error = text
		} else {
			r.Output = text
		}
		t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: r})
	case "auto_retry_end":
		t.mu.Lock()
		defer t.mu.Unlock()
		if ln.Success != nil && *ln.Success {
			t.failed, t.err = false, ""
		} else {
			t.failed = true
			t.err = ln.FinalError
			if t.err == "" {
				t.err = t.s.flavor.name + " exhausted its automatic retries"
			}
		}
	case "extension_error":
		t.mu.Lock()
		t.err = ln.failure(t.s.flavor)
		t.mu.Unlock()
	}
}

// piResultText flattens a tool result: a string, or {content:[{type:text,text}]}, else its JSON.
func piResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		var parts []string
		for _, c := range r.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}

// piSlug splits provider/model, the form Pi's models are named in.
func piSlug(model string) (provider, id string, err error) {
	provider, id, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok || provider == "" || id == "" {
		return "", "", fmt.Errorf("models must use provider/model format; received %q", model)
	}
	return provider, id, nil
}

func (s *piSession) sessionOf(state piLine) string {
	var d struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	_ = json.Unmarshal(state.Data, &d)
	if d.SessionFile != "" {
		return d.SessionFile // resuming needs the path
	}
	return d.SessionID
}

func (p *CLISubprocessProvider) startPi(nt *agent.NativeTurn, f piFlavor) (*piSession, error) {
	name, args := f.command("--mode", "rpc", f.fullAccess)
	// The process outlives the turn that started it, so it isn't bound to the turn's context.
	proc, err := p.start()(context.Background(), name, args, p.Dir)
	if err != nil {
		return nil, err
	}
	s := &piSession{proc: proc, flavor: f, exited: make(chan struct{}), pending: map[string]chan piLine{}}
	go s.read()
	fail := func(err error) (*piSession, error) {
		s.close()
		return nil, fmt.Errorf("starting %s: %w", f.name, err)
	}
	ctx := context.Background()
	if _, err := s.request(ctx, piInitTimeout, map[string]any{"type": "get_state"}); err != nil {
		return fail(err)
	}
	if nt.ResumeID != "" {
		r, err := s.request(ctx, piInitTimeout, map[string]any{"type": "switch_session", "sessionPath": nt.ResumeID})
		if err != nil {
			return fail(err)
		}
		var d struct {
			Cancelled bool `json:"cancelled"`
		}
		if json.Unmarshal(r.Data, &d) == nil && d.Cancelled {
			return fail(errors.New("the session switch was cancelled"))
		}
	}
	if p.Model != "" {
		provider, id, err := piSlug(p.Model)
		if err != nil {
			return fail(err)
		}
		if _, err := s.request(ctx, piInitTimeout, map[string]any{"type": "set_model", "provider": provider, "modelId": id}); err != nil {
			return fail(err)
		}
	}
	if p.effort != "" {
		if _, err := s.request(ctx, piInitTimeout, map[string]any{"type": "set_thinking_level", "level": p.effort}); err != nil {
			return fail(err)
		}
	}
	state, err := s.request(ctx, piInitTimeout, map[string]any{"type": "get_state"})
	if err != nil {
		return fail(err)
	}
	if s.file = s.sessionOf(state); s.file == "" {
		return fail(errors.New("it reported no session id"))
	}
	s.model, s.effort = p.Model, p.effort
	return s, nil
}

func (p *CLISubprocessProvider) runPi(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	f := piFlavorOf(p.Kind)
	if nt.Mode != agent.ModeAutonomous {
		return fmt.Errorf("%s runs with full access only: switch this chat to Run", strings.ToLower(f.name))
	}
	err := p.runPiOnce(ctx, nt, emit, f)
	if errors.Is(err, errStaleSession) {
		err = p.runPiOnce(ctx, nt, emit, f)
	}
	return err
}

func (p *CLISubprocessProvider) runPiOnce(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk), f piFlavor) error {
	usable := func(ns nativeSession) bool {
		s := ns.(*piSession)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.file == nt.ResumeID // a live process serves one conversation; a new one is a new process
	}
	ns, reused, release, err := acquireSession(nt, p.Kind, p.Dir, usable, func() (nativeSession, error) { return p.startPi(nt, f) })
	if err != nil {
		return err
	}
	defer release()
	s := ns.(*piSession)

	t := &piTurn{s: s, nt: nt, emit: emit, done: make(chan struct{}), tools: map[string]string{}}
	s.setTurn(t)
	defer s.setTurn(nil)

	// The model and the thinking level change on the live session.
	s.mu.Lock()
	changeModel, changeEffort := p.Model != "" && p.Model != s.model, p.effort != "" && p.effort != s.effort
	s.mu.Unlock()
	if changeModel {
		provider, id, err := piSlug(p.Model)
		if err != nil {
			return err
		}
		if _, err := s.request(ctx, piRPCTimeout, map[string]any{"type": "set_model", "provider": provider, "modelId": id}); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		s.mu.Lock()
		s.model = p.Model
		s.mu.Unlock()
	}
	if changeEffort {
		if _, err := s.request(ctx, piRPCTimeout, map[string]any{"type": "set_thinking_level", "level": p.effort}); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		s.mu.Lock()
		s.effort = p.effort
		s.mu.Unlock()
	}

	prompt := nt.Prompt
	if nt.ResumeID == "" {
		prompt = nt.FreshPrompt
	}
	s.mu.Lock()
	id := s.newID()
	s.promptID = id
	s.mu.Unlock()
	// Not awaited like other requests: the agent can hold this response until the turn is well
	// under way, and the writer must stay free to send an abort meanwhile.
	if err := s.write(map[string]any{"id": id, "type": "prompt", "message": prompt}); err != nil {
		if reused && s.dead() {
			return errStaleSession
		}
		return fmt.Errorf("%s: %w", f.name, err)
	}
	defer t.over.Store(true)
	nt.SetSteer(func(text string) error {
		if t.over.Load() || s.dead() {
			return errors.New("the turn has ended")
		}
		_, err := s.request(context.Background(), piRPCTimeout, map[string]any{"type": "steer", "message": text})
		return err
	})

	select {
	case <-t.done:
	case <-s.exited:
		select {
		case <-t.done:
		default:
			if reused && !t.seen.Load() {
				return errStaleSession
			}
			msg := f.name + " exited before the turn ended"
			if tail := s.proc.stderr(); tail != "" {
				msg += ": " + lastLines(tail, 5)
			}
			return errors.New(msg)
		}
	case <-ctx.Done():
		// Stop the turn, not the process.
		_, _ = s.request(context.Background(), piRPCTimeout, map[string]any{"type": "abort"})
		select {
		case <-t.done:
		case <-s.exited:
		case <-time.After(10 * time.Second):
			s.close()
		}
		return ctx.Err()
	}

	s.mu.Lock()
	nt.SessionID = s.file
	s.mu.Unlock()
	t.mu.Lock()
	refused, failed, reported := t.refused, t.failed, t.err
	t.mu.Unlock()
	switch {
	case refused != "":
		return fmt.Errorf("%s: %s", strings.ToLower(f.name), refused)
	case failed:
		return fmt.Errorf("%s: %s", strings.ToLower(f.name), reported)
	}
	return nil
}

type piModel struct {
	Provider  string `json:"provider"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Reasoning bool   `json:"reasoning"`
	// Pi maps thinking levels per model; a null entry means the level isn't offered.
	ThinkingLevelMap map[string]json.RawMessage `json:"thinkingLevelMap"`
	// Oh My Pi lists the levels a model honours.
	Thinking struct {
		Efforts []string `json:"efforts"`
	} `json:"thinking"`
}

var piThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (m piModel) efforts(f piFlavor) []string {
	if !m.Reasoning {
		return nil
	}
	var out []string
	for _, level := range piThinkingLevels {
		if f.binary == "omp" {
			// "off" bypasses provider mapping, so it is never listed but always accepted.
			if level == "off" || contains(m.Thinking.Efforts, level) {
				out = append(out, level)
			}
			continue
		}
		mapped, ok := m.ThinkingLevelMap[level]
		isNull := ok && string(mapped) == "null"
		if level == "xhigh" || level == "max" {
			if ok && !isNull {
				out = append(out, level)
			}
		} else if !isNull {
			out = append(out, level)
		}
	}
	return out
}

// discoverPiModels asks a throwaway RPC process for the models it can use.
func discoverPiModels(ctx context.Context, f piFlavor, dir string) ([]ModelInfo, error) {
	name, args := f.command(f.probeArgs...)
	proc, err := discoverProc(ctx, name, args, dir)
	if err != nil {
		return nil, err
	}
	s := &piSession{proc: proc, flavor: f, exited: make(chan struct{}), pending: map[string]chan piLine{}}
	go s.read()
	defer s.close()
	r, err := s.request(ctx, piInitTimeout, map[string]any{"type": "get_available_models"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Models []piModel `json:"models"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, m := range d.Models {
		if m.Provider == "" || m.ID == "" {
			continue
		}
		info := ModelInfo{ID: m.Provider + "/" + m.ID, DisplayName: m.Name}
		if info.DisplayName == "" {
			info.DisplayName = m.ID
		}
		if e := m.efforts(f); len(e) > 0 {
			info.Capabilities = map[string]any{"reasoningEfforts": e}
		}
		out = append(out, info)
	}
	return out, nil
}

// PiModelsDiscovered is the models Pi (or Oh My Pi) offers on this machine, cached.
func PiModelsDiscovered(kind CLIKind, dir string) []ModelInfo {
	f := piFlavorOf(kind)
	return discovered.get(kind, func(ctx context.Context) ([]ModelInfo, error) {
		return discoverPiModels(ctx, f, dir)
	}, func() []ModelInfo { return nil })
}
