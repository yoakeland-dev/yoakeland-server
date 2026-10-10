package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
)

// The Agent Client Protocol (agentclientprotocol.com, v1) — spoken by
// cursor-agent, kimi, opencode, gemini, grok and dsh (SPEC.md §6b). Per
// turn: initialize, session/load (or session/resume, or session/new with a
// transcript), optionally pick model/effort/mode, session/prompt, relaying
// session/update notifications and answering session/request_permission
// until the prompt returns. We advertise no fs/terminal capabilities, so
// the agent uses its own.

type acpToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Content    []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Path    string  `json:"path"`
		OldText *string `json:"oldText"`
		NewText string  `json:"newText"`
	} `json:"content"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

// merge applies a tool_call_update's present fields onto what's known.
func (t *acpToolCall) merge(u acpToolCall) {
	if u.Title != "" {
		t.Title = u.Title
	}
	if u.Name != "" {
		t.Name = u.Name
	}
	if u.Kind != "" {
		t.Kind = u.Kind
	}
	if u.Status != "" {
		t.Status = u.Status
	}
	if len(u.RawInput) > 0 {
		t.RawInput = u.RawInput
	}
	if len(u.RawOutput) > 0 {
		t.RawOutput = u.RawOutput
	}
	if u.Content != nil {
		t.Content = u.Content
	}
	if u.Locations != nil {
		t.Locations = u.Locations
	}
}

func (t *acpToolCall) toolCall() agent.ToolCall {
	name := t.Name
	if name == "" {
		name = t.Kind
	}
	if name == "" {
		name = "tool"
	}
	args := t.RawInput
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	tc := agent.ToolCall{ID: t.ToolCallID, Name: name, Args: args}
	tc.Native, tc.Title, tc.Kind = true, t.Title, t.Kind
	for _, l := range t.Locations {
		tc.Locations = append(tc.Locations, l.Path)
	}
	return tc
}

// plan is the plan text an exit-plan request carries (`plan` in its raw
// input), else its title.
func (t *acpToolCall) plan() string {
	var in struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal(t.RawInput, &in)
	if in.Plan != "" {
		return in.Plan
	}
	return t.Title
}

func (t *acpToolCall) diffs() []*diff.FileDiff {
	var out []*diff.FileDiff
	for _, c := range t.Content {
		if c.Type == "diff" {
			old := ""
			if c.OldText != nil {
				old = *c.OldText
			}
			out = append(out, diff.ComputeDiff(c.Path, old, c.NewText))
		}
	}
	return out
}

func (t *acpToolCall) result() *agent.NativeToolResult {
	r := &agent.NativeToolResult{ToolCallID: t.ToolCallID, Name: t.toolCall().Name, Diffs: t.diffs()}
	var parts []string
	for _, c := range t.Content {
		if c.Type == "content" && c.Content.Type == "text" {
			parts = append(parts, c.Content.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" && len(t.RawOutput) > 0 && string(t.RawOutput) != "null" {
		out = string(t.RawOutput)
	}
	if t.Status == "failed" {
		r.Error = out
	} else {
		r.Output = out
	}
	return r
}

type acpSession struct {
	SessionID string `json:"sessionId"`
	Modes     *struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"availableModes"`
	} `json:"modes"`
	ConfigOptions []acpConfigOption `json:"configOptions"`
	Models        *struct {
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	} `json:"models"`
}

type acpConfigOption struct {
	ID           string          `json:"id"`
	Category     string          `json:"category"`
	CurrentValue string          `json:"currentValue"`
	Options      json.RawMessage `json:"options"`
}

// values flattens a select option's choices (plain or grouped).
func (o acpConfigOption) values() []ModelInfo {
	type opt struct {
		Value   string `json:"value"`
		Name    string `json:"name"`
		Options []opt  `json:"options"`
	}
	var opts []opt
	_ = json.Unmarshal(o.Options, &opts)
	var out []ModelInfo
	var walk func([]opt)
	walk = func(os []opt) {
		for _, x := range os {
			if x.Value != "" {
				out = append(out, ModelInfo{ID: x.Value, DisplayName: x.Name})
			}
			walk(x.Options)
		}
	}
	walk(opts)
	return out
}

func (s acpSession) option(category string) *acpConfigOption {
	for i := range s.ConfigOptions {
		if s.ConfigOptions[i].Category == category {
			return &s.ConfigOptions[i]
		}
	}
	return nil
}

// modeChoices are the session's modes, from its "mode" config option when it
// has one (set with session/set_config_option under that option's own id),
// else from the older `modes` block (session/set_mode). current is the one
// the agent is in now.
func (s acpSession) modeChoices() (choices []acpModeChoice, configID, current string) {
	if o := s.option("mode"); o != nil {
		for _, v := range o.values() {
			choices = append(choices, acpModeChoice{v.ID, v.DisplayName})
		}
		return choices, o.ID, o.CurrentValue
	}
	if s.Modes != nil {
		for _, m := range s.Modes.AvailableModes {
			choices = append(choices, acpModeChoice{m.ID, m.Name})
		}
		return choices, "", s.Modes.CurrentModeID
	}
	return nil, "", ""
}

// acpPlanPrefix tells an agent with no plan mode of its own to plan.
func acpPlanPrefix(s acpSession, m agent.Mode) string {
	choices, _, _ := s.modeChoices()
	if _, ok := pickACPMode(choices, agent.ModePlan); ok {
		return ""
	}
	return planPrefix(m)
}

// applyMode puts the agent in its own mode for m. No match leaves it where
// it is; asks are still gated by agent.askNativePermission either way.
func (s acpSession) applyMode(ctx context.Context, conn *rpcConn, m agent.Mode) {
	choices, configID, current := s.modeChoices()
	id, ok := pickACPMode(choices, m)
	if !ok {
		if len(choices) > 0 {
			log.Printf("acp: no mode for %q among the agent's %v; yoakeland's permission checks still apply", m, choices)
		}
		return
	}
	if id == current {
		return
	}
	var err error
	if configID != "" {
		_, err = conn.call(ctx, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": configID, "value": id})
	} else {
		_, err = conn.call(ctx, "session/set_mode", map[string]any{"sessionId": s.SessionID, "modeId": id})
	}
	if err != nil {
		log.Printf("acp: setting mode %q failed: %v", id, err)
	}
}

// acpInit starts an ACP agent and runs initialize, returning the connection
// and whether it supports session/load. The process is bound to procCtx, the
// initialize call to ctx.
func (p *CLISubprocessProvider) acpInit(ctx, procCtx context.Context, spec cliSpec, onNotify func(string, json.RawMessage), onRequest func(*rpcConn, json.RawMessage, string, json.RawMessage)) (*rpcConn, acpCaps, error) {
	proc, err := p.start()(procCtx, spec.Binary, spec.Args, p.Dir)
	if err != nil {
		return nil, acpCaps{}, err
	}
	var conn *rpcConn
	conn = newRPCConn(proc, onNotify, func(id json.RawMessage, method string, params json.RawMessage) { onRequest(conn, id, method, params) })
	raw, err := conn.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "yoakeland", "version": "1"},
	})
	if err != nil {
		// The process is bound to procCtx, not ctx: stop it here or it outlives the failed start.
		_ = proc.stdin.Close()
		if proc.kill != nil {
			proc.kill()
		}
		return nil, acpCaps{}, fmt.Errorf("%s: %w", spec.Binary, err)
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession     bool `json:"loadSession"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &init)
	c := init.AgentCapabilities
	return conn, acpCaps{loadSession: c.LoadSession, mcpHTTP: c.MCPCapabilities.HTTP, mcpSSE: c.MCPCapabilities.SSE}, nil
}

// acpCaps is what an ACP agent said it can do in initialize.
type acpCaps struct {
	loadSession     bool
	mcpHTTP, mcpSSE bool
}

// acpErr makes the common "not logged in" failure actionable.
func acpErr(binary string, err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "auth") {
		return fmt.Errorf("%s: sign in first by running `%s` once in a terminal on the server (%v)", binary, binary, err)
	}
	return err
}

// acpLive is one live ACP agent process: one ACP session at a time, prompted
// once per turn. Its notifications and requests go to the turn in progress.
type acpLive struct {
	conn    *rpcConn
	spec    cliSpec
	canLoad bool
	caps    acpCaps

	mu      sync.Mutex
	turn    *acpTurn
	loading bool // session/load replays history as updates; drop them
	sess    acpSession
	model   string // what has been set on sess, so a turn only sends a change
	effort  string
	mode    agent.Mode
	// baseline is the cost usage_update reported while history replayed: what a
	// resumed session had already spent.
	baseline float64
}

func (a *acpLive) dead() bool {
	select {
	case <-a.conn.done:
		return true
	default:
		return false
	}
}

func (a *acpLive) close() {
	_ = a.conn.p.stdin.Close()
	select {
	case <-a.conn.done:
	case <-time.After(3 * time.Second):
		if a.conn.p.kill != nil {
			a.conn.p.kill()
		}
	}
}

func (a *acpLive) setTurn(t *acpTurn) {
	a.mu.Lock()
	a.turn = t
	a.mu.Unlock()
}

func (a *acpLive) current() (*acpTurn, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn, a.loading
}

// acpTurn is what one turn has seen of the session's output so far.
type acpTurn struct {
	a    *acpLive
	ctx  context.Context
	nt   *agent.NativeTurn
	emit func(agent.ModelStreamChunk)

	seen atomic.Bool // the agent has said something this turn

	mu    sync.Mutex
	calls map[string]*acpToolCall
	done  map[string]bool
	cost  float64 // latest cumulative usage_update cost in USD, -1 = none seen
	// used and size are the latest usage_update's context tokens in use and
	// the context window; 0 = none seen.
	used, size int
}

func (t *acpTurn) seenAnything() bool { return t.seen.Load() }

func (a *acpLive) onNotify(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var n struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			// An object for message chunks, an array for tool calls.
			Content json.RawMessage `json:"content"`
			Cost    *struct {
				Amount   float64 `json:"amount"`
				Currency string  `json:"currency"`
			} `json:"cost"`
			Used int `json:"used"`
			Size int `json:"size"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	u := n.Update
	t, replaying := a.current()
	if replaying {
		if u.SessionUpdate == "usage_update" && u.Cost != nil && strings.EqualFold(u.Cost.Currency, "USD") {
			a.mu.Lock()
			a.baseline = u.Cost.Amount
			a.mu.Unlock()
		}
		return
	}
	if t == nil {
		return
	}
	if u.SessionUpdate == "usage_update" && (u.Used > 0 || u.Size > 0) {
		t.mu.Lock()
		t.used, t.size = u.Used, u.Size
		t.mu.Unlock()
	}
	t.update(u.SessionUpdate, u.Content, u.Cost, params)
}

func (t *acpTurn) update(kind string, content json.RawMessage, cost *struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}, params json.RawMessage) {
	t.seen.Store(true)
	if kind == "usage_update" && cost != nil && strings.EqualFold(cost.Currency, "USD") {
		t.mu.Lock()
		t.cost = cost.Amount
		t.mu.Unlock()
	}
	var text struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &text)
	switch kind {
	case "agent_message_chunk":
		if text.Type == "text" {
			t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text.Text})
		}
	case "agent_thought_chunk":
		if text.Type == "text" {
			t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: text.Text})
		}
	case "tool_call", "tool_call_update":
		var c struct {
			Update acpToolCall `json:"update"`
		}
		if json.Unmarshal(params, &c) != nil || c.Update.ToolCallID == "" {
			return
		}
		t.mu.Lock()
		known, seen := t.calls[c.Update.ToolCallID]
		if !seen {
			known = &acpToolCall{ToolCallID: c.Update.ToolCallID}
			t.calls[c.Update.ToolCallID] = known
		}
		known.merge(c.Update)
		snapshot := *known
		finished := (snapshot.Status == "completed" || snapshot.Status == "failed") && !t.done[snapshot.ToolCallID]
		if finished {
			t.done[snapshot.ToolCallID] = true
		}
		t.mu.Unlock()
		if !seen {
			tc := snapshot.toolCall()
			t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
		}
		if finished {
			t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: snapshot.result()})
		}
	}
}

func (a *acpLive) onRequest(conn *rpcConn, id json.RawMessage, method string, params json.RawMessage) {
	t, _ := a.current()
	if t == nil {
		conn.respond(id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		return
	}
	t.request(conn, id, method, params)
}

func (t *acpTurn) request(conn *rpcConn, id json.RawMessage, method string, params json.RawMessage) {
	if method != "session/request_permission" {
		conn.respondError(id, -32601, "yoakeland doesn't provide "+method)
		return
	}
	var r struct {
		ToolCall acpToolCall `json:"toolCall"`
		Options  []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &r)
	t.mu.Lock()
	if known := t.calls[r.ToolCall.ToolCallID]; known != nil {
		known.merge(r.ToolCall)
		r.ToolCall = *known
	}
	t.mu.Unlock()
	var d *diff.FileDiff
	if ds := r.ToolCall.diffs(); len(ds) > 0 {
		d = ds[0]
	}
	desc := r.ToolCall.Title
	if desc == "" {
		desc = string(r.ToolCall.RawInput)
	}
	ask := agent.NativeAsk{ToolCallID: r.ToolCall.ToolCallID, ToolName: r.ToolCall.toolCall().Name, Description: desc, Diff: d}
	switch r.ToolCall.Kind {
	case "edit", "delete", "move":
		ask.Edit = true
	case "switch_mode":
		// The agent leaving its plan mode: what it wants approved is the plan.
		ask.Plan = r.ToolCall.plan()
	}
	ans, err := t.nt.Ask(t.ctx, ask)
	if err != nil {
		conn.respond(id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
		return
	}
	prefer := []string{"reject_once", "reject_always"}
	switch {
	case ans.Allowed && ans.Remember:
		prefer = []string{"allow_always", "allow_once"}
	case ans.Allowed:
		prefer = []string{"allow_once", "allow_always"}
	case ans.Remember:
		prefer = []string{"reject_always", "reject_once"}
	}
	for _, k := range prefer {
		for _, o := range r.Options {
			if o.Kind == k {
				conn.respond(id, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": o.OptionID}})
				return
			}
		}
	}
	conn.respond(id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
}

func (p *CLISubprocessProvider) startACP(spec cliSpec) (*acpLive, error) {
	a := &acpLive{spec: spec}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The process outlives the turn that started it, so it isn't bound to the turn's context.
	conn, caps, err := p.acpInit(ctx, context.Background(), spec, a.onNotify, a.onRequest)
	if err != nil {
		return nil, acpErr(spec.Binary, err)
	}
	a.conn, a.canLoad, a.caps = conn, caps.loadSession, caps
	return a, nil
}

func (p *CLISubprocessProvider) runACP(ctx context.Context, spec cliSpec, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	err := p.runACPOnce(ctx, spec, nt, emit)
	if errors.Is(err, errStaleSession) {
		err = p.runACPOnce(ctx, spec, nt, emit)
	}
	return err
}

func (p *CLISubprocessProvider) runACPOnce(ctx context.Context, spec cliSpec, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	ns, reused, release, err := acquireSession(nt, p.Kind, p.Dir, func(nativeSession) bool { return true },
		func() (nativeSession, error) { return p.startACP(spec) })
	if err != nil {
		return err
	}
	defer release()
	a := ns.(*acpLive)
	conn := a.conn
	failed := func(err error) error {
		if reused && a.dead() && ctx.Err() == nil {
			return errStaleSession
		}
		return acpErr(spec.Binary, err)
	}

	t := &acpTurn{a: a, ctx: ctx, nt: nt, emit: emit, calls: map[string]*acpToolCall{}, done: map[string]bool{}, cost: -1}
	a.setTurn(t)
	defer a.setTurn(nil)

	a.mu.Lock()
	sess := a.sess
	a.mu.Unlock()
	prompt := nt.Prompt
	baseline := nt.SessionCostUSD
	if sess.SessionID == "" || sess.SessionID != nt.ResumeID {
		// A different conversation from the one this process is on (or none yet).
		sess = acpSession{}
		resumed := false
		if nt.ResumeID != "" {
			params := map[string]any{"sessionId": nt.ResumeID, "cwd": p.Dir, "mcpServers": acpMCPServers(nt.MCP, a.caps)}
			method := "session/resume" // no history replay
			if a.canLoad {
				method = "session/load"
				a.mu.Lock()
				a.loading, a.baseline = true, baseline
				a.mu.Unlock()
			}
			raw, err := conn.call(ctx, method, params)
			a.mu.Lock()
			a.loading = false
			baseline = a.baseline
			a.mu.Unlock()
			if err == nil {
				_ = json.Unmarshal(raw, &sess)
				sess.SessionID, resumed = nt.ResumeID, true
			} else if a.dead() {
				return failed(err)
			}
		}
		if !resumed {
			prompt = nt.FreshPrompt
			baseline = 0
			raw, err := conn.call(ctx, "session/new", map[string]any{"cwd": p.Dir, "mcpServers": acpMCPServers(nt.MCP, a.caps)})
			if err != nil {
				return failed(err)
			}
			if err := json.Unmarshal(raw, &sess); err != nil || sess.SessionID == "" {
				return fmt.Errorf("%s session/new: no session id", spec.Binary)
			}
		}
		a.mu.Lock()
		a.sess, a.model, a.effort, a.mode = sess, "", "", ""
		a.mu.Unlock()
	}
	nt.SessionID = sess.SessionID

	// Model/effort/mode are best-effort: an agent that rejects one still runs.
	// Only what changed since the session last had it set is sent.
	a.mu.Lock()
	applyModel, applyEffort := p.Model != "" && p.Model != a.model, p.effort != "" && p.effort != a.effort
	applyMode := nt.Mode != a.mode
	a.model, a.effort, a.mode = firstNonEmpty(p.Model, a.model), firstNonEmpty(p.effort, a.effort), nt.Mode
	a.mu.Unlock()
	if o := sess.option("model"); o != nil && applyModel {
		_, _ = conn.call(ctx, "session/set_config_option", map[string]any{"sessionId": sess.SessionID, "configId": o.ID, "value": p.Model})
	} else if sess.Models != nil && applyModel {
		_, _ = conn.call(ctx, "session/set_model", map[string]any{"sessionId": sess.SessionID, "modelId": p.Model})
	}
	if o := sess.option("thought_level"); o != nil && applyEffort {
		_, _ = conn.call(ctx, "session/set_config_option", map[string]any{"sessionId": sess.SessionID, "configId": o.ID, "value": p.effort})
	}
	if applyMode {
		sess.applyMode(ctx, conn, nt.Mode)
	}

	// A steer is a second session/prompt while one is open: the agent carries on
	// with the newer request and the earlier one is answered early (some agents
	// call that "cancelled"), so only the last prompt's answer settles the turn.
	// Each prompt's answer is waited for even when the turn is stopped, so the
	// next turn never overlaps one on the same process.
	type promptResult struct {
		raw json.RawMessage
		err error
	}
	var (
		pmu     sync.Mutex
		prompts []chan promptResult
		issued  = make(chan struct{}, 1)
		over    atomic.Bool
	)
	sendPrompt := func(text string) {
		ch := make(chan promptResult, 1)
		pmu.Lock()
		prompts = append(prompts, ch)
		pmu.Unlock()
		go func() {
			raw, err := conn.call(context.Background(), "session/prompt", map[string]any{
				"sessionId": sess.SessionID,
				"prompt":    []map[string]string{{"type": "text", "text": text}},
			})
			ch <- promptResult{raw, err}
		}()
	}
	last := func() chan promptResult {
		pmu.Lock()
		defer pmu.Unlock()
		return prompts[len(prompts)-1]
	}
	sendPrompt(acpPlanPrefix(sess, nt.Mode) + prompt)
	defer over.Store(true)
	if !spec.NoSteer {
		nt.SetSteer(func(text string) error {
			if over.Load() || a.dead() {
				return errors.New("the turn has ended")
			}
			sendPrompt(text)
			select {
			case issued <- struct{}{}:
			default:
			}
			return nil
		})
	}
	var res promptResult
wait:
	for {
		cur := last()
		select {
		case r := <-cur:
			if cur == last() {
				res = r
				break wait
			}
		case <-issued:
		case <-ctx.Done():
			conn.notify("session/cancel", map[string]string{"sessionId": sess.SessionID})
			deadline := time.After(10 * time.Second)
			pmu.Lock()
			open := append([]chan promptResult(nil), prompts...)
			pmu.Unlock()
			for _, ch := range open {
				select {
				case <-ch:
				case <-deadline:
					a.close()
					return ctx.Err()
				}
			}
			return ctx.Err()
		}
	}
	if res.err != nil {
		if !t.seenAnything() {
			return failed(res.err)
		}
		return acpErr(spec.Binary, res.err)
	}
	var resp struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens       int `json:"inputTokens"`
			OutputTokens      int `json:"outputTokens"`
			CachedReadTokens  int `json:"cachedReadTokens"`
			CachedWriteTokens int `json:"cachedWriteTokens"`
		} `json:"usage"` // unstable ACP; reported by some agents only
	}
	_ = json.Unmarshal(res.raw, &resp)

	usage := &protocol.UsageData{}
	if u := resp.Usage; u != nil {
		usage.InputTokens = u.InputTokens + u.CachedReadTokens + u.CachedWriteTokens
		usage.OutputTokens = u.OutputTokens
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		usage.CachedInputTokens = u.CachedReadTokens
	}
	t.mu.Lock()
	// The prompt's own usage sums its requests; usage_update says how full
	// the context is.
	usage.ContextTokens, usage.WindowTokens = t.used, t.size
	if t.cost >= 0 {
		nt.SessionCostUSD = t.cost
		if t.cost >= baseline {
			usage.CostUSD, usage.CostKnown = t.cost-baseline, true
		}
	}
	t.mu.Unlock()
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: usage})

	switch resp.StopReason {
	case "max_tokens":
		emit(agent.ModelStreamChunk{Kind: agent.StreamChunkFinish, FinishReason: "length"})
	case "refusal":
		return fmt.Errorf("%s refused to continue", spec.Binary)
	case "cancelled":
		return context.Canceled
	}
	return nil
}

func firstNonEmpty[T ~string](a, b T) T {
	if a != "" {
		return a
	}
	return b
}

// ListACPModels asks an ACP agent which models it offers, by opening a
// throwaway session (ACP has no models call outside a session).
func ListACPModels(ctx context.Context, kind CLIKind, dir string) ([]ModelInfo, error) {
	spec, ok := cliSpecs[kind]
	if !ok || spec.Protocol != "acp" {
		return nil, fmt.Errorf("%q is not an ACP agent", kind)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &CLISubprocessProvider{Kind: kind, Dir: dir}
	conn, _, err := p.acpInit(ctx, ctx, spec, func(string, json.RawMessage) {}, func(c *rpcConn, id json.RawMessage, m string, _ json.RawMessage) {
		c.respondError(id, -32601, "not supported")
	})
	if err != nil {
		return nil, acpErr(spec.Binary, err)
	}
	defer func() { _ = conn.p.stdin.Close() }()
	raw, err := conn.call(ctx, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}})
	if err != nil {
		return nil, acpErr(spec.Binary, err)
	}
	var sess acpSession
	_ = json.Unmarshal(raw, &sess)
	choices, _, _ := sess.modeChoices()
	modes := acpModes(choices)
	if o := sess.option("model"); o != nil {
		return withPermissionModes(o.values(), modes), nil
	}
	var out []ModelInfo
	if sess.Models != nil {
		for _, m := range sess.Models.AvailableModels {
			out = append(out, ModelInfo{ID: m.ModelID, DisplayName: m.Name})
		}
	}
	return withPermissionModes(out, modes), nil
}
