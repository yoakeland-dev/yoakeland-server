package tool_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

func TestFileWriteAndEdit_WithLiveSnapshotRollback(t *testing.T) {
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

	// 1. Write file
	_, err = writeTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"content": "Hello World\nLine 2"
	}`))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// 2. Edit file
	_, err = editTool.Execute(context.Background(), execCtx, []byte(`{
		"path": "hello.txt",
		"oldString": "World",
		"newString": "Yoakeland"
	}`))
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(tempDir, "hello.txt"))
	if string(content) != "Hello Yoakeland\nLine 2" {
		t.Fatalf("unexpected content after edit: %q", string(content))
	}

	// 3. Verify snapshots were recorded
	snaps, err := snapStore.ListSnapshots()
	if err != nil || len(snaps) < 2 {
		t.Fatalf("expected at least 2 snapshots recorded, got %d", len(snaps))
	}

	// 4. Restore the snapshot captured before the edit
	editSnap := snaps[len(snaps)-1]
	if err := snapStore.RestoreSnapshot(editSnap.ID, tempDir); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	restoredContent, _ := os.ReadFile(filepath.Join(tempDir, "hello.txt"))
	if string(restoredContent) != "Hello World\nLine 2" {
		t.Fatalf("expected content restored to pre-edit state, got %q", string(restoredContent))
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
