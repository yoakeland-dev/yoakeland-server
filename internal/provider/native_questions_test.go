package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"yoakeland/internal/agent"
)

// Claude's AskUserQuestion carries several questions; they're handed over in
// one batch so the user sees them all at once.
func TestClaudeQuestionsAskedTogether(t *testing.T) {
	var batches []int
	var reply map[string]any
	rec := &turnRecorder{}
	nt := rec.turn(agent.ModeReadWrite)
	nt.Questions = func(_ context.Context, qs []agent.NativeQuestion) ([]string, error) {
		batches = append(batches, len(qs))
		return []string{"a", "b"}, nil
	}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir()}
	var args []string
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		in.Scan()
		line(out, map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "q1", "input": map[string]any{"questions": []any{
			map[string]any{"question": "one?", "options": []any{map[string]any{"label": "x"}}},
			map[string]any{"question": "two?"},
		}}}})
		in.Scan()
		_ = json.Unmarshal(in.Bytes(), &reply)
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "ok"})
	})
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	wait()
	if len(batches) != 1 || batches[0] != 2 {
		t.Fatalf("batches = %v", batches)
	}
	body := reply["response"].(map[string]any)["response"].(map[string]any)
	answers := body["updatedInput"].(map[string]any)["answers"].(map[string]any)
	if answers["one?"] != "a" || answers["two?"] != "b" {
		t.Fatalf("answers = %v", answers)
	}
}
