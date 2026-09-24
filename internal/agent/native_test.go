package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// fakeNative plays a CLI agent: it records the NativeTurn it got, asks one
// permission, then reports text → tool call → result → text → usage.
type fakeNative struct {
	turns   []agent.NativeTurn
	answers []agent.NativeAnswer
}

func (f *fakeNative) RunsOwnTools() {}

func (f *fakeNative) StreamTurn(ctx context.Context, _ []agent.Message, tools []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk, 8), make(chan error, 1)
	nt := agent.NativeTurnFromContext(ctx)
	go func() {
		defer close(c)
		defer close(e)
		ans, _ := nt.Ask(ctx, agent.NativeAsk{ToolCallID: "call-1", ToolName: "Bash", Description: "ls"})
		f.answers = append(f.answers, ans)
		f.turns = append(f.turns, *nt)
		nt.SessionID = "native-1"
		tc := agent.ToolCall{ID: "call-1", Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}
		tc.Native, tc.Title, tc.Kind = true, "ls", "execute"
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "Looking."}
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc}
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: &agent.NativeToolResult{ToolCallID: "call-1", Name: "Bash", Output: "a.txt"}}
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "Found a.txt."}
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5, CostKnown: true}}
	}()
	return c, e
}

// capturingAPI records the history a plain model provider is sent.
type capturingAPI struct{ got []agent.Message }

func (p *capturingAPI) StreamTurn(_ context.Context, h []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	p.got = h
	c, e := make(chan agent.ModelStreamChunk, 1), make(chan error)
	c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ok"}
	close(c)
	close(e)
	return c, e
}

func TestAgent_NativeTurnRecordsStepsResumesAndFlattens(t *testing.T) {
	native := &fakeNative{}
	ag, store, _ := testAgent(t, native)
	t.Cleanup(ag.WaitBackground)
	ag.SetProvider(native, "cc")
	ag.SetMode(agent.ModeAutonomous) // asks auto-allowed, no human needed

	if err := ag.RunTurn(context.Background(), "s", "t1", "first"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	st, _ := store.Load("s")
	var roles []string
	for _, m := range st.History {
		roles = append(roles, m.Role)
	}
	// Stored in the order it happened: the call's message, its result, then the rest.
	if got := strings.Join(roles, ","); got != "user,assistant,tool,assistant" {
		t.Fatalf("history roles = %s", got)
	}
	if tc := st.History[1].ToolCalls; len(tc) != 1 || !tc[0].Native || st.History[1].Content != "Looking." {
		t.Fatalf("call message = %+v", st.History[1])
	}
	if u := st.History[3].Usage; u == nil || u.CostUSD != 0.5 {
		t.Fatalf("usage on final message = %+v", u)
	}
	if st.Native == nil || st.Native.SessionID != "native-1" || st.Native.Cursor != st.History[3].ID {
		t.Fatalf("native ref = %+v", st.Native)
	}
	if !native.answers[0].Allowed || native.turns[0].ResumeID != "" || native.turns[0].Prompt != "first" {
		t.Fatalf("turn 1 = %+v, answer %+v", native.turns[0], native.answers[0])
	}

	// Unchanged history: resume the native session with just the new message.
	ag.SetMode(agent.ModeReadOnly) // and read-only denies the agent's ask
	_ = ag.RunTurn(context.Background(), "s", "t2", "second")
	if nt := native.turns[1]; nt.ResumeID != "native-1" || nt.Prompt != "second" || !strings.Contains(nt.FreshPrompt, "User: first") {
		t.Fatalf("turn 2 = %+v", nt)
	}
	if native.answers[1].Allowed {
		t.Fatal("read-only mode must deny a native agent's permission ask")
	}

	// An API provider gets the native steps as text, never as tool calls.
	api := &capturingAPI{}
	ag.SetProvider(api, "api")
	_ = ag.RunTurn(context.Background(), "s", "t3", "third")
	for _, m := range api.got {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("API provider was sent a native tool step: %+v", m)
		}
	}
	if !strings.Contains(api.got[1].Content, "[tool call Bash ls]") || !strings.Contains(api.got[1].Content, "[tool result] a.txt") {
		t.Fatalf("flattened step = %q", api.got[1].Content)
	}

	// Back on the native agent after other turns: the cursor no longer
	// matches, so it starts fresh from a transcript.
	ag.SetProvider(native, "cc")
	_ = ag.RunTurn(context.Background(), "s", "t4", "fourth")
	if nt := native.turns[2]; nt.ResumeID != "" || !strings.Contains(nt.Prompt, "User: third") {
		t.Fatalf("turn 4 = %+v", nt)
	}
}
