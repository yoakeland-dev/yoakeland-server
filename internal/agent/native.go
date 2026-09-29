package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// NativeAgent is a provider that is a whole coding agent (the claude/codex/
// ACP CLIs, SPEC.md §6b): it runs its own tools, sandbox and session, so the
// turn loop hands it the prompt and relays what it reports — tool calls and
// results arrive as chunks already executed, permission asks and questions
// arrive through the NativeTurn in ctx — instead of offering it yoakeland's
// own tool set. yoakeland's built-in tools are for the plain model APIs.
type NativeAgent interface {
	ModelProvider
	RunsOwnTools()
}

// NativeToolResult is a NativeAgent's report of a tool call it finished.
type NativeToolResult struct {
	ToolCallID string
	// ParentToolCallID: see protocol.NativeToolInfo.
	ParentToolCallID string
	Name             string
	Output           string
	Error            string
	Diffs            []*diff.FileDiff
}

// NativeAsk is a NativeAgent asking permission for one of its tool calls.
type NativeAsk struct {
	ToolCallID  string
	ToolName    string
	Description string
	Diff        *diff.FileDiff
	// Edit: the ask is for a file change (Accept Edits lets those through).
	Edit bool
	// Plan, when set, makes this the agent asking to leave its plan mode with
	// this plan: it is shown to the user, who approves or sends it back.
	Plan string
}

// NativeQuestion is one question a NativeAgent asks the user.
type NativeQuestion struct {
	ToolCallID string
	Question   string
	Options    []string
}

// NativeAnswer is the user's verdict. Remember maps onto the agent's own
// "always allow/deny" option, where it has one — yoakeland's
// permissions.json only covers yoakeland's own tools.
type NativeAnswer struct {
	Allowed  bool
	Remember bool
	Feedback string
}

// NativeSessionRef is the CLI agent's own session this chat continues, and
// Cursor is the ID of the last message that session has seen. A turn resumes
// it only while the chat's history still ends at Cursor; after an edit,
// retry, branch switch, or provider change the history has moved on without
// it, so the next turn starts a fresh native session seeded with a
// transcript instead.
type NativeSessionRef struct {
	Provider  string  `json:"provider"`
	SessionID string  `json:"sessionId"`
	Cursor    string  `json:"cursor"`
	CostUSD   float64 `json:"costUsd,omitempty"` // see NativeTurn.SessionCostUSD
}

// NativeTurn is what a NativeAgent gets for one turn, via ctx.
type NativeTurn struct {
	Mode     Mode
	ResumeID string // native session to continue; "" starts a new one
	Prompt   string // the new message when resuming, else a transcript + it
	// FreshPrompt is always the transcript + new message: what to send
	// instead of Prompt if resuming ResumeID turns out to be impossible.
	FreshPrompt string
	// SessionCostUSD is the native session's cumulative cost before this
	// turn, for agents that only report a running total (ACP usage_update);
	// the provider leaves the new total here.
	SessionCostUSD float64

	// Ask blocks until the user answers a permission request (or ctx ends).
	Ask func(ctx context.Context, req NativeAsk) (NativeAnswer, error)
	// Questions asks all of them at once, so a client can stack their cards,
	// and blocks until every one is answered; answers come back in order.
	Questions func(ctx context.Context, qs []NativeQuestion) ([]string, error)

	// SessionID is set by the provider once its native session id is known.
	SessionID string
}

type nativeTurnKey struct{}

// WithNativeTurn is how the turn loop (and tests) hand a NativeAgent its turn.
func WithNativeTurn(ctx context.Context, nt *NativeTurn) context.Context {
	return context.WithValue(ctx, nativeTurnKey{}, nt)
}

// NativeTurnFromContext returns the turn a NativeAgent's StreamTurn serves.
func NativeTurnFromContext(ctx context.Context) *NativeTurn {
	nt, _ := ctx.Value(nativeTurnKey{}).(*NativeTurn)
	return nt
}

// newNativeTurn decides resume-vs-fresh from a.native and builds the prompt.
// history already ends with the user message this turn answers.
func (a *Agent) newNativeTurn(sessionID, turnID, providerName string, history []Message) *NativeTurn {
	nt := &NativeTurn{Mode: a.Mode()}
	last := len(history) - 1
	for last >= 0 && history[last].Role != "user" {
		last--
	}
	if last < 0 {
		return nt
	}
	msg := history[last].Content + attachmentLine(history[last].Attachments)
	a.mu.Lock()
	ref := a.native
	a.mu.Unlock()
	nt.FreshPrompt = msg
	if last > 0 {
		nt.FreshPrompt = "Earlier in this conversation (you don't have this in your context, it's a transcript):\n\n" +
			renderTranscript(contextView(history[:last], "")) + "\n---\n\n" + msg
	}
	nt.Prompt = nt.FreshPrompt
	if ref != nil && ref.Provider == providerName && last > 0 && history[last-1].ID == ref.Cursor {
		nt.ResumeID, nt.Prompt, nt.SessionCostUSD = ref.SessionID, msg, ref.CostUSD
	}
	nt.Ask = func(ctx context.Context, req NativeAsk) (NativeAnswer, error) {
		return a.askNativePermission(ctx, sessionID, turnID, req)
	}
	nt.Questions = func(ctx context.Context, qs []NativeQuestion) ([]string, error) {
		return a.askUserQuestions(ctx, sessionID, turnID, qs)
	}
	return nt
}

func attachmentLine(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "\n\n[Attached files: " + strings.Join(paths, ", ") + "]"
}

// renderTranscript flattens history to text: for seeding a fresh native
// session, and (via flattenNativeTools) for replaying native tool steps to
// a provider that can't take another agent's tool calls.
func renderTranscript(history []Message) string {
	var sb strings.Builder
	for _, m := range history {
		switch m.Role {
		case "user":
			fmt.Fprintf(&sb, "User: %s%s\n", m.Content, attachmentLine(m.Attachments))
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&sb, "Assistant: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "[tool call %s %s]\n", tc.Name, toolCallLabel(tc))
			}
		case "tool":
			fmt.Fprintf(&sb, "[tool result] %s\n", truncateRunes(m.Content, 2000))
		}
	}
	return sb.String()
}

func toolCallLabel(tc ToolCall) string {
	if tc.Title != "" {
		return tc.Title
	}
	return truncateRunes(string(tc.Args), 500)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// flattenNativeTools rewrites native tool steps as plain assistant text, for
// a provider that isn't the agent that ran them: an API model can't be sent
// tool_use blocks for tools it was never offered (Anthropic rejects them),
// and a different CLI agent has its own tool names anyway.
func flattenNativeTools(history []Message) []Message {
	native := map[string]bool{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.Native {
				native[tc.ID] = true
			}
		}
	}
	if len(native) == 0 {
		return history
	}
	out := make([]Message, 0, len(history))
	for _, m := range history {
		switch {
		case m.Role == "tool" && native[m.ToolCallID]:
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].Content += "\n[tool result] " + truncateRunes(m.Content, 2000)
			}
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var keep []ToolCall
			text := m.Content
			for _, tc := range m.ToolCalls {
				if tc.Native {
					text += fmt.Sprintf("\n[tool call %s %s]", tc.Name, toolCallLabel(tc))
				} else {
					keep = append(keep, tc)
				}
			}
			m.Content, m.ToolCalls = strings.TrimPrefix(text, "\n"), keep
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	return out
}

// askNativePermission turns a native agent's permission ask into the same
// permission_request flow yoakeland's own tools use, gated by mode.
func (a *Agent) askNativePermission(ctx context.Context, sessionID, turnID string, req NativeAsk) (NativeAnswer, error) {
	mode := a.Mode()
	if req.Plan != "" {
		a.recordPlan(sessionID, turnID, req.ToolCallID, req.Plan)
		if mode == ModeAutonomous {
			return NativeAnswer{Allowed: true}, nil
		}
		req.Description = "Approve the plan"
	} else {
		switch {
		case mode == ModeAutonomous, mode == ModeAcceptEdits && req.Edit:
			return NativeAnswer{Allowed: true}, nil
		case mode == ModeReadOnly, mode == ModePlan:
			return NativeAnswer{Feedback: "not allowed in " + string(mode) + " mode"}, nil
		}
	}
	respCh := make(chan permDecision, 1)
	a.mu.Lock()
	a.permRequests[req.ToolCallID] = respCh
	a.nativeRemember[req.ToolCallID] = false
	a.mu.Unlock()
	data := protocol.PermissionRequestData{
		ToolName:    req.ToolName,
		Description: req.Description,
		RequiresAsk: true,
		Diff:        req.Diff,
		Plan:        req.Plan != "",
	}
	a.addPendingAsk(protocol.PendingAsk{Kind: "permission", SessionID: sessionID, TurnID: turnID, ToolCallID: req.ToolCallID, PermissionRequestData: &data})
	_ = a.getStream().SendEvent(protocol.EventPermissionRequest, sessionID, turnID, req.ToolCallID, data)
	select {
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.permRequests, req.ToolCallID)
		delete(a.nativeRemember, req.ToolCallID)
		a.mu.Unlock()
		a.removePendingAsk(req.ToolCallID)
		return NativeAnswer{}, ctx.Err()
	case d := <-respCh:
		a.mu.Lock()
		remember := a.nativeRemember[req.ToolCallID]
		delete(a.nativeRemember, req.ToolCallID)
		a.mu.Unlock()
		return NativeAnswer{Allowed: d.allowed, Remember: remember, Feedback: d.feedback}, nil
	}
}

// askUserQuestion is the ask_user_question round trip for yoakeland's own
// tool: one question.
func (a *Agent) askUserQuestion(ctx context.Context, sessionID, turnID, toolCallID, question string, options []string) (string, error) {
	answers, err := a.askUserQuestions(ctx, sessionID, turnID, []NativeQuestion{{toolCallID, question, options}})
	if err != nil {
		return "", err
	}
	return answers[0], nil
}

// askUserQuestions raises every question before waiting on any, in order, so
// they are all pending together and the client stacks them.
func (a *Agent) askUserQuestions(ctx context.Context, sessionID, turnID string, qs []NativeQuestion) ([]string, error) {
	chans := make([]chan string, len(qs))
	for i, nq := range qs {
		chans[i] = make(chan string, 1)
		a.mu.Lock()
		a.askUserRequests[nq.ToolCallID] = chans[i]
		a.mu.Unlock()
		q := protocol.AskUserQuestionRequestedData{Question: nq.Question, Options: nq.Options}
		a.addPendingAsk(protocol.PendingAsk{Kind: "question", SessionID: sessionID, TurnID: turnID, ToolCallID: nq.ToolCallID, AskUserQuestionRequestedData: &q})
		_ = a.getStream().SendEvent(protocol.EventAskUserQuestionRequested, sessionID, turnID, nq.ToolCallID, q)
	}
	answers := make([]string, len(qs))
	for i := range qs {
		select {
		case <-ctx.Done():
			for _, nq := range qs[i:] {
				a.mu.Lock()
				delete(a.askUserRequests, nq.ToolCallID)
				a.mu.Unlock()
				a.removePendingAsk(nq.ToolCallID)
			}
			return nil, ctx.Err()
		case answers[i] = <-chans[i]:
		}
	}
	return answers, nil
}

// attachUsage puts a native turn's usage on message id.
func (a *Agent) attachUsage(id string, usage protocol.UsageData) {
	if id == "" || usage == (protocol.UsageData{}) {
		return
	}
	if !usage.CostKnown {
		usage.CostUSD = EstimateCostUSD(a.getModelName(), usage)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].ID == id {
			a.history[i].Usage = &usage
			return
		}
	}
}

// MarshalArgs is a small helper providers use to turn a decoded input back
// into ToolCall.Args.
func MarshalArgs(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return json.RawMessage("{}")
	}
	return b
}

// recordPlan shows content as the session's plan (plan_written), through the
// same tool an API model's plan goes through, and notes that this turn has one.
func (a *Agent) recordPlan(sessionID, turnID, toolCallID, content string) {
	t, ok := a.tools["write_plan"]
	if !ok || strings.TrimSpace(content) == "" {
		return
	}
	args, _ := json.Marshal(map[string]string{"content": content})
	_, _ = t.Execute(context.Background(), tool.ExecutionContext{SessionID: sessionID, TurnID: turnID, ToolCallID: toolCallID, Stream: a.getStream()}, args)
	a.mu.Lock()
	a.planTurn = turnID
	a.mu.Unlock()
}

func (a *Agent) planRecorded(turnID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planTurn == turnID
}

// ResolvePlanApproval approves a native agent's pending request to leave plan
// mode. False when there isn't one (an API model's plan is just done).
func (a *Agent) ResolvePlanApproval() bool {
	for _, ask := range a.PendingAsks() {
		if ask.PermissionRequestData != nil && ask.Plan {
			a.HandlePermissionResponse(ask.ToolCallID, true, false, "")
			return true
		}
	}
	return false
}
