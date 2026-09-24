package snapshot_test

import (
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/snapshot"
)

func TestDiskSnapshotStore_CaptureAndRestore(t *testing.T) {
	projectRoot := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(projectRoot)
	if err != nil {
		t.Fatalf("failed to init snapshot store: %v", err)
	}

	existingFile := filepath.Join(projectRoot, "existing.txt")
	if err := os.WriteFile(existingFile, []byte("initial state"), 0644); err != nil {
		t.Fatal(err)
	}

	newFile := filepath.Join(projectRoot, "newfile.txt")

	// Capture snapshot covering existing and non-existing file
	snap, captured, err := store.CapturePreMutation("turn-1", "test write", projectRoot, existingFile, newFile)
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}
	if !captured {
		t.Fatal("expected captured to be true for a first-time capture")
	}

	// Mutate filesystem
	_ = os.WriteFile(existingFile, []byte("mutated state"), 0644)
	_ = os.WriteFile(newFile, []byte("brand new content"), 0644)

	// Verify mutations took place
	curExist, _ := os.ReadFile(existingFile)
	if string(curExist) != "mutated state" {
		t.Fatalf("expected mutated state")
	}

	// Restore snapshot
	if err := store.RestoreSnapshot(snap.ID, projectRoot); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	// Verify existing was restored
	restored, err := os.ReadFile(existingFile)
	if err != nil || string(restored) != "initial state" {
		t.Fatalf("expected 'initial state', got %q", string(restored))
	}

	// Verify newly created file was cleaned up
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("newly created file should have been removed on restore")
	}
}

// TestDiskSnapshotStore_CapturePreMutation_GroupsByTurn guards SPEC.md §6's
// per-turn restore contract: two CapturePreMutation calls sharing a turnID
// but touching different files must land in the *same* snapshot, so one
// RestoreSnapshot call reverts everything the turn touched — not just
// whichever single mutation happened to create the snapshot.
func TestDiskSnapshotStore_CapturePreMutation_GroupsByTurn(t *testing.T) {
	projectRoot := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(projectRoot)
	if err != nil {
		t.Fatalf("failed to init snapshot store: %v", err)
	}

	fileA := filepath.Join(projectRoot, "a.txt")
	fileB := filepath.Join(projectRoot, "b.txt")
	if err := os.WriteFile(fileA, []byte("a-original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("b-original"), 0644); err != nil {
		t.Fatal(err)
	}

	snap1, captured1, err := store.CapturePreMutation("turn-x", "file_write: a.txt", projectRoot, fileA)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if !captured1 {
		t.Fatal("expected first capture to report captured=true")
	}
	_ = os.WriteFile(fileA, []byte("a-mutated"), 0644)

	snap2, captured2, err := store.CapturePreMutation("turn-x", "file_write: b.txt", projectRoot, fileB)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if !captured2 {
		t.Fatal("expected second capture to report captured=true")
	}
	_ = os.WriteFile(fileB, []byte("b-mutated"), 0644)

	if snap1.ID != snap2.ID {
		t.Fatalf("expected both captures in turn-x to share one snapshot ID, got %q and %q", snap1.ID, snap2.ID)
	}
	if len(snap2.Files) != 2 {
		t.Fatalf("expected the shared snapshot to track both files, got %d", len(snap2.Files))
	}
	wantDesc := "file_write: a.txt; file_write: b.txt"
	if snap2.Description != wantDesc {
		t.Errorf("Description = %q, want %q", snap2.Description, wantDesc)
	}

	// One restore call must revert both files.
	if err := store.RestoreSnapshot(snap2.ID, projectRoot); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gotA, _ := os.ReadFile(fileA)
	gotB, _ := os.ReadFile(fileB)
	if string(gotA) != "a-original" {
		t.Errorf("fileA = %q, want %q", gotA, "a-original")
	}
	if string(gotB) != "b-original" {
		t.Errorf("fileB = %q, want %q", gotB, "b-original")
	}

	store.CloseTurn("turn-x")

	// A later, unrelated turn must not extend the closed one.
	fileC := filepath.Join(projectRoot, "c.txt")
	_ = os.WriteFile(fileC, []byte("c-original"), 0644)
	snap3, _, err := store.CapturePreMutation("turn-y", "file_write: c.txt", projectRoot, fileC)
	if err != nil {
		t.Fatalf("third capture: %v", err)
	}
	if snap3.ID == snap2.ID {
		t.Fatal("expected a new turn to start a new snapshot, not extend a closed one")
	}
}

// TestDiskSnapshotStore_CapturePreMutation_SecondCaptureOfSameFileIsNoop
// guards against a second mutation of the same file within one turn
// silently overwriting the *pre-turn* backup with an already-mutated
// intermediate state — restoring the turn's snapshot must still produce the
// state from before the turn started, not the state after its first edit.
func TestDiskSnapshotStore_CapturePreMutation_SecondCaptureOfSameFileIsNoop(t *testing.T) {
	projectRoot := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(projectRoot)
	if err != nil {
		t.Fatalf("failed to init snapshot store: %v", err)
	}

	file := filepath.Join(projectRoot, "f.txt")
	if err := os.WriteFile(file, []byte("pre-turn"), 0644); err != nil {
		t.Fatal(err)
	}

	snap1, captured1, err := store.CapturePreMutation("turn-z", "first edit", projectRoot, file)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if !captured1 {
		t.Fatal("expected first capture to report captured=true")
	}
	_ = os.WriteFile(file, []byte("after-first-edit"), 0644)

	// Second mutation of the SAME file, same turn — must not re-capture.
	snap2, captured2, err := store.CapturePreMutation("turn-z", "second edit", projectRoot, file)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if captured2 {
		t.Fatal("expected captured=false when the file was already tracked this turn")
	}
	if snap2.ID != snap1.ID {
		t.Fatal("expected the second capture to reuse the same snapshot")
	}
	_ = os.WriteFile(file, []byte("after-second-edit"), 0644)

	if err := store.RestoreSnapshot(snap2.ID, projectRoot); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "pre-turn" {
		t.Fatalf("restored content = %q, want the original pre-turn state %q", got, "pre-turn")
	}
}

func TestDiskSnapshotStore_RestoreSnapshot_RejectsTraversalID(t *testing.T) {
	projectRoot := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(projectRoot)
	if err != nil {
		t.Fatalf("failed to init snapshot store: %v", err)
	}

	for _, id := range []string{
		"../../../../etc",
		"..",
		"foo/../../bar",
		"/etc/passwd",
		"a/b",
		"",
	} {
		if err := store.RestoreSnapshot(id, projectRoot); err == nil {
			t.Fatalf("expected RestoreSnapshot to reject traversal id %q, got nil error", id)
		}
		if _, err := store.GetSnapshot(id); err == nil {
			t.Fatalf("expected GetSnapshot to reject traversal id %q, got nil error", id)
		}
	}
}

func TestDiskSnapshotStore_RestoreSnapshot_RejectsTraversalRelPath(t *testing.T) {
	projectRoot := t.TempDir()
	storeDir := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(storeDir)
	if err != nil {
		t.Fatalf("failed to init snapshot store: %v", err)
	}

	// Craft a snapshot manifest by hand whose relPath escapes projectRoot,
	// followed by restore_snapshot: it must not write outside projectRoot.
	snapDir := filepath.Join(storeDir, "snapshots", "evil")
	dataDir := filepath.Join(snapDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}

	outsideTarget := filepath.Join(filepath.Dir(projectRoot), "escaped.txt")
	defer os.Remove(outsideTarget)

	payload := filepath.Join(dataDir, "payload.bak")
	if err := os.WriteFile(payload, []byte("pwned"), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"id": "evil",
		"description": "malicious",
		"createdAt": "2026-01-01T00:00:00Z",
		"files": {
			"../escaped.txt": {
				"relPath": "../escaped.txt",
				"existed": true,
				"fileMode": 420,
				"backupRelLoc": "data/payload.bak"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	if err := store.RestoreSnapshot("evil", projectRoot); err == nil {
		t.Fatalf("expected RestoreSnapshot to reject an escaping relPath, got nil error")
	}
	if _, err := os.Stat(outsideTarget); !os.IsNotExist(err) {
		t.Fatalf("restore must not have written outside projectRoot, but %s exists", outsideTarget)
	}
}
