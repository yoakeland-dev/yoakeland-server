package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

func TestWritePlanTool_WritesPerSessionFileAndEmitsEvent(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	wt := tool.NewWritePlanTool(norm)

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	execCtx := tool.ExecutionContext{
		SessionID: "session-123",
		TurnID:    "turn-1",
		Stream:    ipc,
	}

	args, _ := json.Marshal(tool.WritePlanArgs{Content: "# The Plan\n\nStep one."})
	out, err := wt.Execute(context.Background(), execCtx, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, ".yoake/plans/session-123.md") {
		t.Fatalf("expected the result to name the written path, got: %q", out)
	}

	written, err := os.ReadFile(filepath.Join(tempDir, ".yoake", "plans", "session-123.md"))
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	if string(written) != "# The Plan\n\nStep one." {
		t.Fatalf("plan file content mismatch: %q", written)
	}

	if !strings.Contains(outBuf.String(), "plan_written") || !strings.Contains(outBuf.String(), "session-123.md") {
		t.Fatalf("expected a plan_written event naming the path, got: %s", outBuf.String())
	}
}

// TestWritePlanTool_TwoSessionsDoNotClobber guards the reason a per-session
// path was chosen over a single per-project plan.md: two concurrent
// Plan-mode sessions in the same project must not overwrite each other.
func TestWritePlanTool_TwoSessionsDoNotClobber(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	wt := tool.NewWritePlanTool(norm)

	argsA, _ := json.Marshal(tool.WritePlanArgs{Content: "plan A"})
	argsB, _ := json.Marshal(tool.WritePlanArgs{Content: "plan B"})

	if _, err := wt.Execute(context.Background(), tool.ExecutionContext{SessionID: "session-A"}, argsA); err != nil {
		t.Fatalf("Execute A: %v", err)
	}
	if _, err := wt.Execute(context.Background(), tool.ExecutionContext{SessionID: "session-B"}, argsB); err != nil {
		t.Fatalf("Execute B: %v", err)
	}

	a, _ := os.ReadFile(filepath.Join(tempDir, ".yoake", "plans", "session-A.md"))
	b, _ := os.ReadFile(filepath.Join(tempDir, ".yoake", "plans", "session-B.md"))
	if string(a) != "plan A" || string(b) != "plan B" {
		t.Fatalf("expected independent per-session plan files, got A=%q B=%q", a, b)
	}
}
