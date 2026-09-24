package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"yoakeland/internal/agent"
)

// Deleting .yoake while the server runs must not break later saves.
func TestDiskSessionStore_SaveAfterYoakeDirDeleted(t *testing.T) {
	root := t.TempDir()
	store, err := agent.NewDiskSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".yoake")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&agent.SessionState{SessionID: "s1"}); err != nil {
		t.Fatalf("Save after .yoake was deleted: %v", err)
	}
	if _, err := store.Load("s1"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
