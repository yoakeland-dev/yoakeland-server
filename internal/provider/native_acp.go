package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
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
// and whether it supports session/load.
func (p *CLISubprocessProvider) acpInit(ctx context.Context, spec cliSpec, onNotify func(string, json.RawMessage), onRequest func(*rpcConn, json.RawMessage, string, json.RawMessage)) (*rpcConn, bool, error) {
	proc, err := p.start()(ctx, spec.Binary, spec.Args, p.Dir)
	if err != nil {
		return nil, false, err
	}
	var conn *rpcConn
	conn = newRPCConn(proc, onNotify, func(id json.RawMessage, method string, params json.RawMessage) { onRequest(conn, id, method, params) })
	raw, err := conn.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "yoakeland", "version": "1"},
	})
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", spec.Binary, err)
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &init)
	return conn, init.AgentCapabilities.LoadSession, nil
}

// acpErr makes the common "not logged in" failure actionable.
func acpErr(binary string, err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "auth") {
		return fmt.Errorf("%s needs you to sign in first: run `%s` once in a terminal on the server (%v)", binary, binary, err)
	}
	return err
}

func (p *CLISubprocessProvider) runACP(ctx context.Context, spec cliSpec, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	var (
		mu       sync.Mutex
		loading  bool // session/load replays history as updates; drop them
		calls    = map[string]*acpToolCall{}
		done     = map[string]bool{}
		cost     = -1.0 // latest cumulative usage_update cost in USD, -1 = none seen
		baseline = nt.SessionCostUSD
	)
	onNotify := func(method string, params json.RawMessage) {
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
			} `json:"update"`
		}
		if json.Unmarshal(params, &n) != nil {
			return
		}
		u := n.Update
		mu.Lock()
		replaying := loading
		if u.SessionUpdate == "usage_update" && u.Cost != nil && strings.EqualFold(u.Cost.Currency, "USD") {
			if replaying {
				baseline = u.Cost.Amount
			} else {
				cost = u.Cost.Amount
			}
		}
		mu.Unlock()
		if replaying {
			return
		}
		var text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(u.Content, &text)
		switch u.SessionUpdate {
		case "agent_message_chunk":
			if text.Type == "text" {
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text.Text})
			}
		case "agent_thought_chunk":
			if text.Type == "text" {
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: text.Text})
			}
		case "tool_call", "tool_call_update":
			var t struct {
				Update acpToolCall `json:"update"`
			}
			if json.Unmarshal(params, &t) != nil || t.Update.ToolCallID == "" {
				return
			}
			mu.Lock()
			known, seen := calls[t.Update.ToolCallID]
			if !seen {
				known = &acpToolCall{ToolCallID: t.Update.ToolCallID}
				calls[t.Update.ToolCallID] = known
			}
			known.merge(t.Update)
			snapshot := *known
			finished := (snapshot.Status == "completed" || snapshot.Status == "failed") && !done[snapshot.ToolCallID]
			if finished {
				done[snapshot.ToolCallID] = true
			}
			mu.Unlock()
			if !seen {
				tc := snapshot.toolCall()
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
			}
			if finished {
				emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: snapshot.result()})
			}
		}
	}
	onRequest := func(conn *rpcConn, id json.RawMessage, method string, params json.RawMessage) {
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
		mu.Lock()
		if known := calls[r.ToolCall.ToolCallID]; known != nil {
			known.merge(r.ToolCall)
			r.ToolCall = *known
		}
		mu.Unlock()
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
		ans, err := nt.Ask(ctx, ask)
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

	conn, canLoad, err := p.acpInit(ctx, spec, onNotify, onRequest)
	if err != nil {
		return acpErr(spec.Binary, err)
	}
	defer func() { _ = conn.p.stdin.Close() }()

	base := map[string]any{"cwd": p.Dir, "mcpServers": []any{}}
	var sess acpSession
	prompt := nt.Prompt
	resumed := false
	if nt.ResumeID != "" {
		params := map[string]any{"sessionId": nt.ResumeID, "cwd": p.Dir, "mcpServers": []any{}}
		method := "session/resume" // no history replay
		if canLoad {
			method = "session/load"
			mu.Lock()
			loading = true
			mu.Unlock()
		}
		raw, err := conn.call(ctx, method, params)
		mu.Lock()
		loading = false
		mu.Unlock()
		if err == nil {
			_ = json.Unmarshal(raw, &sess)
			sess.SessionID, resumed = nt.ResumeID, true
		}
	}
	if !resumed {
		prompt = nt.FreshPrompt
		mu.Lock()
		baseline = 0
		mu.Unlock()
		raw, err := conn.call(ctx, "session/new", base)
		if err != nil {
			return acpErr(spec.Binary, err)
		}
		if err := json.Unmarshal(raw, &sess); err != nil || sess.SessionID == "" {
			return fmt.Errorf("%s session/new: no session id", spec.Binary)
		}
	}
	nt.SessionID = sess.SessionID

	// Model/effort/mode are best-effort: an agent that rejects one still runs.
	if o := sess.option("model"); o != nil && p.Model != "" {
		_, _ = conn.call(ctx, "session/set_config_option", map[string]any{"sessionId": sess.SessionID, "configId": o.ID, "value": p.Model})
	} else if sess.Models != nil && p.Model != "" {
		_, _ = conn.call(ctx, "session/set_model", map[string]any{"sessionId": sess.SessionID, "modelId": p.Model})
	}
	if o := sess.option("thought_level"); o != nil && p.effort != "" {
		_, _ = conn.call(ctx, "session/set_config_option", map[string]any{"sessionId": sess.SessionID, "configId": o.ID, "value": p.effort})
	}
	sess.applyMode(ctx, conn, nt.Mode)

	raw, err := conn.call(ctx, "session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": acpPlanPrefix(sess, nt.Mode) + prompt}},
	})
	if err != nil {
		if ctx.Err() != nil {
			conn.notify("session/cancel", map[string]string{"sessionId": sess.SessionID})
			return ctx.Err()
		}
		return acpErr(spec.Binary, err)
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
	_ = json.Unmarshal(raw, &resp)

	usage := &protocol.UsageData{}
	if u := resp.Usage; u != nil {
		usage.InputTokens = u.InputTokens + u.CachedReadTokens + u.CachedWriteTokens
		usage.OutputTokens = u.OutputTokens
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		usage.CachedInputTokens = u.CachedReadTokens
	}
	mu.Lock()
	if cost >= 0 {
		nt.SessionCostUSD = cost
		if cost >= baseline {
			usage.CostUSD, usage.CostKnown = cost-baseline, true
		}
	}
	mu.Unlock()
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
	conn, _, err := p.acpInit(ctx, spec, func(string, json.RawMessage) {}, func(c *rpcConn, id json.RawMessage, m string, _ json.RawMessage) {
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
