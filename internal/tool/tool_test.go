package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// TestFileWriteAndEdit_SameTurn_OneSnapshotRevertsWholeTurn guards the
// per-turn snapshot grouping: a write followed by an edit of the same file
// within one turn must land in a single snapshot, and restoring it must
// revert the *entire turn* — including undoing the write itself, since the
// file didn't exist before the turn started — not just the most recent
// mutation.
func TestFileWriteAndEdit_SameTurn_OneSnapshotRevertsWholeTurn(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	writeTool := tool.NewFileWriteTool(norm)
	editTool := tool.NewFileEditTool(norm)

	execCtx := tool.ExecutionContext{
		SessionID:     "s1",
		TurnID:        "t1",
		ToolCallID:    "c1",
		SnapshotStore: snapStore,
	}

	if _, err := writeTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"content": "Hello World\nLine 2"
	}`)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	if _, err := editTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"oldString": "World",
		"newString": "Yoakeland"
	}`)); err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(tempDir, "hello.txt"))
	if string(content) != "Hello Yoakeland\nLine 2" {
		t.Fatalf("unexpected content after edit: %q", string(content))
	}

	snaps, err := snapStore.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected exactly 1 snapshot for one turn's write+edit, got %d", len(snaps))
	}

	if err := snapStore.RestoreSnapshot(snaps[0].ID, tempDir); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempDir, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected the whole turn to be reverted (file didn't exist pre-turn), but it exists")
	}
}

// TestFileWriteAndEdit_DifferentTurns_IndependentSnapshots guards the
// complementary case: a write in one turn followed by an edit in a
// *different* turn produces two independent snapshots, so restoring only
// the edit's turn reverts just that edit, leaving the earlier write intact.
func TestFileWriteAndEdit_DifferentTurns_IndependentSnapshots(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	writeTool := tool.NewFileWriteTool(norm)
	editTool := tool.NewFileEditTool(norm)

	writeCtx := tool.ExecutionContext{SessionID: "s1", TurnID: "turn-1", ToolCallID: "c1", SnapshotStore: snapStore}
	editCtx := tool.ExecutionContext{SessionID: "s1", TurnID: "turn-2", ToolCallID: "c2", SnapshotStore: snapStore}

	if _, err := writeTool.Execute(context.Background(), writeCtx, []byte(`{
		"path": "hello.txt",
		"content": "Hello World\nLine 2"
	}`)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	// The turn ends (as Agent.RunTurn's defer would do) before the next one starts.
	snapStore.CloseTurn("turn-1")

	if _, err := editTool.Execute(context.Background(), editCtx, []byte(`{
		"path": "hello.txt",
		"oldString": "World",
		"newString": "Yoakeland"
	}`)); err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	snaps, err := snapStore.ListSnapshots()
	if err != nil || len(snaps) != 2 {
		t.Fatalf("expected 2 independent snapshots across 2 turns, got %d (err=%v)", len(snaps), err)
	}

	// Restore only the edit's snapshot (the more recently created one).
	editSnap := snaps[0]
	if snaps[1].CreatedAt.After(editSnap.CreatedAt) {
		editSnap = snaps[1]
	}
	if err := snapStore.RestoreSnapshot(editSnap.ID, tempDir); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	restoredContent, _ := os.ReadFile(filepath.Join(tempDir, "hello.txt"))
	if string(restoredContent) != "Hello World\nLine 2" {
		t.Fatalf("expected content restored to the post-write/pre-edit state, got %q", string(restoredContent))
	}
}

// TestFileWrite_EmitsSnapshotCapturingThenCreated guards the wire visibility
// added around pre-mutation snapshot capture: a successful write must emit
// snapshot_capturing before the (normally invisible) capture work, then
// snapshot_created once it's done — in that order.
func TestFileWrite_EmitsSnapshotCapturingThenCreated(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	outBuf := &bytes.Buffer{}
	stream := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)

	writeTool := tool.NewFileWriteTool(norm)
	execCtx := tool.ExecutionContext{SessionID: "s1", TurnID: "t1", ToolCallID: "c1", SnapshotStore: snapStore, Stream: stream}

	if _, err := writeTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"content": "hi"
	}`)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	types := envelopeTypes(t, outBuf.String())
	wantSeq := []string{"snapshot_capturing", "snapshot_created"}
	if len(types) < len(wantSeq) {
		t.Fatalf("expected at least %v, got %v", wantSeq, types)
	}
	for i, want := range wantSeq {
		if types[i] != want {
			t.Fatalf("event sequence = %v, want it to start with %v", types, wantSeq)
		}
	}
}

// TestFileWrite_CaptureFailure_EmitsErrorEvent guards that a snapshot
// capture failure is surfaced as its own distinct wire event (in addition
// to the tool call's own error return, which is unchanged) rather than
// being silently swallowed until some later point.
func TestFileWrite_CaptureFailure_EmitsErrorEvent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based write denial doesn't apply the same way on windows")
	}

	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	// Make the snapshot store's base directory unwritable so the next
	// capture's os.MkdirAll/os.WriteFile inside it fails deterministically.
	snapshotsDir := filepath.Join(tempDir, "snapshots")
	if err := os.Chmod(snapshotsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(snapshotsDir, 0o755) //nolint:errcheck // best-effort cleanup so t.TempDir() can remove it

	outBuf := &bytes.Buffer{}
	stream := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)

	writeTool := tool.NewFileWriteTool(norm)
	execCtx := tool.ExecutionContext{SessionID: "s1", TurnID: "t1", ToolCallID: "c1", SnapshotStore: snapStore, Stream: stream}

	_, err = writeTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"content": "hi"
	}`))
	if err == nil {
		t.Fatal("expected the write to fail when the snapshot store can't write")
	}

	if !strings.Contains(outBuf.String(), `"type":"error"`) {
		t.Fatalf("expected an error event on capture failure, got: %s", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "snapshot capture failed") {
		t.Fatalf("expected the error event to identify the snapshot capture, got: %s", outBuf.String())
	}
}

// envelopeTypes decodes each newline-delimited envelope in raw and returns
// their "type" fields in order.
func envelopeTypes(t *testing.T, raw string) []string {
	t.Helper()
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("decoding envelope line %q: %v", line, err)
		}
		types = append(types, env.Type)
	}
	return types
}

// TestFileWrite_RefusesSymlinkedTempFile guards the O_NOFOLLOW fix: the
// atomic-write temp path (safePath + ".tmp") is never itself passed through
// normalizer.ResolveAndValidatePath (only the real target path is), so a
// pre-planted symlink at that exact temp-file name — from a malicious repo
// checkout, or a concurrent tool call — must not be silently followed and
// written through to wherever it points, possibly outside the project root.
func TestFileWrite_RefusesSymlinkedTempFile(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "escaped.txt")
	if err := os.WriteFile(outsideTarget, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	tmpPath := filepath.Join(tempDir, "hello.txt.tmp")
	if err := os.Symlink(outsideTarget, tmpPath); err != nil {
		t.Fatal(err)
	}

	writeTool := tool.NewFileWriteTool(norm)
	_, err = writeTool.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{
		"path": "hello.txt",
		"content": "pwned"
	}`))
	if err == nil {
		t.Fatal("expected file_write to refuse a symlinked temp path, got nil error")
	}

	got, readErr := os.ReadFile(outsideTarget)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "original" {
		t.Fatalf("write escaped through the symlink: outside file now contains %q", string(got))
	}
}

func TestFileReadAndBash(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)

	_ = os.WriteFile(filepath.Join(tempDir, "data.txt"), []byte("read me"), 0644)
	rTool := tool.NewFileReadTool(norm)
	out, err := rTool.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"path":"data.txt"}`))
	if err != nil || out != "read me" {
		t.Fatalf("read failed: %v, out: %s", err, out)
	}

	bTool := tool.NewBashTool(norm, sb)
	bOut, err := bTool.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"command":"echo test"}`))
	if err != nil || bOut != "test\n" {
		t.Fatalf("bash failed: %v, out: %s", err, bOut)
	}
}

// TestBashTool_InjectsGitAuthorEnvWhenKnown guards the git-attribution
// wiring: a bash call carrying ExecutionContext.AuthorName gets
// GIT_AUTHOR_NAME/EMAIL (and the COMMITTER equivalents) in its environment,
// and a call with no known author gets neither — so an unattributed (e.g.
// unix-socket, no registered token) turn leaves the sandbox's ambient git
// config untouched instead of injecting an empty/placeholder identity.
func TestBashTool_InjectsGitAuthorEnvWhenKnown(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	bTool := tool.NewBashTool(norm, sb)

	// argv runs directly (no shell), so $VAR interpolation isn't a thing
	// here — printenv reads the actual process environment instead.
	out, err := bTool.Execute(context.Background(), tool.ExecutionContext{
		AuthorName:  "aveline",
		AuthorEmail: "aveline@example.com",
	}, []byte(`{"command":"printenv GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL"}`))
	if err != nil {
		t.Fatalf("bash failed: %v", err)
	}
	want := "aveline\naveline@example.com\naveline\naveline@example.com\n"
	if out != want {
		t.Fatalf("bash output = %q, want %q", out, want)
	}

	out, err = bTool.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"command":"printenv GIT_AUTHOR_NAME"}`))
	if err != nil {
		t.Fatalf("bash failed: %v", err)
	}
	if !strings.Contains(out, "Command failed") {
		t.Fatalf("bash output with no known author = %q, want printenv to fail (exit 1) since GIT_AUTHOR_NAME is unset", out)
	}
}
