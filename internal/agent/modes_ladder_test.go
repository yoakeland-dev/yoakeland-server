package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/tool"
)

// Accept Edits: a file edit goes through unasked, a command still asks.
func TestAgent_AcceptEditsAllowsEditsAndAsksForBash(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	write, _ := json.Marshal(map[string]string{"path": "a.txt", "content": "hi"})
	bash, _ := json.Marshal(map[string]string{"command": "pwd"})
	provider := &capturingProvider{turns: [][]agent.ModelStreamChunk{
		{
			{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "w1", Name: "file_write", Args: write}},
			{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "b1", Name: "bash", Args: bash}},
		},
		{{Kind: agent.StreamChunkDelta, Delta: "done"}},
	}}
	ag, out := newTestAgent(t, dir, provider, []tool.Tool{tool.NewFileWriteTool(norm), tool.NewBashTool(norm, sandbox.NewSandbox(dir, 5*time.Second))})
	ag.SetMode(agent.ModeAcceptEdits)

	go func() {
		waitForAsk(t, ag, "b1", 5*time.Second)
		ag.HandlePermissionResponse("b1", false, false, "")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t1", "go"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the edit should have gone through unasked: %q, %v", b, err)
	}
	if n := strings.Count(out.String(), `"type":"permission_request"`); n != 1 {
		t.Fatalf("want exactly one permission_request (bash), got %d: %s", n, out.String())
	}
}

// A native agent leaving plan mode: its plan is shown, the ask waits for
// approve_plan, and nothing else is allowed while planning.
func TestAgent_NativePlanApproval(t *testing.T) {
	dir := t.TempDir()
	ag, out := newTestAgent(t, dir, &capturingProvider{}, []tool.Tool{tool.NewWritePlanTool(filepath.Join(dir, "plans"))})
	ag.SetMode(agent.ModePlan)

	if ans, _ := ag.AskNative(context.Background(), "s1", "t1", agent.NativeAsk{ToolCallID: "e1", ToolName: "Edit", Edit: true}); ans.Allowed {
		t.Fatal("an edit must be refused in plan mode")
	}
	type result struct {
		ans agent.NativeAnswer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := ag.AskNative(context.Background(), "s1", "t1", agent.NativeAsk{ToolCallID: "x1", ToolName: "ExitPlanMode", Plan: "# Do the thing"})
		done <- result{ans, err}
	}()
	waitForSubstring(t, out, "plan_written", 2*time.Second)
	waitForSubstring(t, out, `"plan":true`, 2*time.Second)
	if !strings.Contains(out.String(), "# Do the thing") {
		t.Fatalf("the plan text should be in plan_written: %s", out.String())
	}
	if !ag.ResolvePlanApproval() {
		t.Fatal("expected a pending plan approval")
	}
	if r := <-done; r.err != nil || !r.ans.Allowed {
		t.Fatalf("approval = %+v, %v", r.ans, r.err)
	}
	if ag.ResolvePlanApproval() {
		t.Fatal("nothing should be pending anymore")
	}
}

// Accept Edits also lets a native agent's file changes through and asks for the rest.
func TestAgent_AcceptEditsNativeAsks(t *testing.T) {
	ag, out := newTestAgent(t, t.TempDir(), &capturingProvider{}, nil)
	ag.SetMode(agent.ModeAcceptEdits)
	if ans, err := ag.AskNative(context.Background(), "s1", "t1", agent.NativeAsk{ToolCallID: "e1", ToolName: "Edit", Edit: true}); err != nil || !ans.Allowed {
		t.Fatalf("edit ask: %+v %v", ans, err)
	}
	go func() {
		waitForSubstring(t, out, `"toolCallId":"c1"`, 2*time.Second)
		ag.HandlePermissionResponse("c1", false, false, "no")
	}()
	if ans, _ := ag.AskNative(context.Background(), "s1", "t1", agent.NativeAsk{ToolCallID: "c1", ToolName: "Bash"}); ans.Allowed {
		t.Fatal("a command must still ask")
	}
}

// nativeText is a native agent that just answers with text.
type nativeText struct{ capturingProvider }

func (nativeText) RunsOwnTools() {}

// A native agent with no plan tool of its own still ends a plan turn with a
// plan: its final message.
func TestAgent_NativePlanTurnRecordsFinalMessage(t *testing.T) {
	dir := t.TempDir()
	p := &nativeText{capturingProvider{turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "# Steps\n1. do it"}}}}}
	ag, out := newTestAgent(t, dir, p, []tool.Tool{tool.NewWritePlanTool(filepath.Join(dir, "plans"))})
	ag.SetMode(agent.ModePlan)
	if err := ag.RunTurn(context.Background(), "s1", "t1", "plan it"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"type":"plan_written"`) || !strings.Contains(got, "1. do it") {
		t.Fatalf("expected the final message as plan_written: %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "plans", "s1.md")); err != nil || !strings.Contains(string(b), "do it") {
		t.Fatalf("plan file: %q %v", b, err)
	}
}

// steerNative works until it is steered, then finishes what it was saying.
type steerNative struct{ ready, steered chan string }

func (steerNative) RunsOwnTools() {}

func (s steerNative) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk, 8), make(chan error, 1)
	nt := agent.NativeTurnFromContext(ctx)
	got := make(chan string, 1)
	nt.SetSteer(func(text string) error { got <- text; return nil })
	go func() {
		defer close(c)
		defer close(e)
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "before"}
		s.ready <- "working"
		select {
		case text := <-got:
			s.steered <- text
		case <-ctx.Done():
			return
		}
		time.Sleep(50 * time.Millisecond) // a model round-trip, which the steer is recorded well inside of
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "after"}
	}()
	return c, e
}

// A message steered into a native turn is recorded where it happened: after
// what the agent had said, before what it said next.
func TestAgent_SteerIsRecordedInOrder(t *testing.T) {
	native := steerNative{ready: make(chan string, 1), steered: make(chan string, 1)}
	ag, store, out := testAgent(t, native)
	ag.SetProvider(native, "cc")

	if err := ag.Steer("t1", "too early", agent.Author{}); err == nil {
		t.Fatal("a steer with no turn running was accepted")
	}
	errc := make(chan error, 1)
	go func() { errc <- ag.RunTurn(context.Background(), "s1", "t1", "go") }()
	<-native.ready

	if err := ag.Steer("other-turn", "x", agent.Author{}); err == nil {
		t.Fatal("a steer for another turn was accepted")
	}
	if err := ag.Steer("t1", "also do x", agent.Author{Name: "ada"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if got := <-native.steered; got != "also do x" {
		t.Fatalf("the agent was sent %q", got)
	}
	st, _ := store.Load("s1")
	var got []string
	for _, m := range st.History {
		got = append(got, m.Role+":"+m.Content)
	}
	if want := "user:go|assistant:before|user:also do x|assistant:after"; strings.Join(got, "|") != want {
		t.Fatalf("history = %s\nwant      %s", strings.Join(got, "|"), want)
	}
	if st.History[2].AuthorName != "ada" {
		t.Fatalf("author = %q", st.History[2].AuthorName)
	}
	if !strings.Contains(out.String(), `"type":"steer_accepted"`) || !strings.Contains(out.String(), `"prompt":"also do x"`) {
		t.Fatalf("no steer_accepted: %s", out.String())
	}
	if err := ag.Steer("t1", "too late", agent.Author{}); err == nil {
		t.Fatal("a steer after the turn ended was accepted")
	}
}
