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
	snap, err := store.CapturePreMutation("test write", projectRoot, existingFile, newFile)
	if err != nil {
		t.Fatalf("capture failed: %v", err)
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
