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
