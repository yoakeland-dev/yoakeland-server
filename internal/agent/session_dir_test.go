package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"yoakeland/internal/agent"
)

// Deleting a project's data dir while the server runs must not break every
// later save (it did: the sessions dir was only created at startup).
func TestDiskSessionStore_SaveAfterDataDirDeleted(t *testing.T) {
	root := t.TempDir()
	store, err := agent.NewDiskSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&agent.SessionState{SessionID: "s1"}); err != nil {
		t.Fatalf("Save after the data dir was deleted: %v", err)
	}
	if _, err := store.Load("s1"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Session ids come off the wire; one with a separator must never become a
// path outside the sessions folder.
func TestDiskSessionStore_RejectsTraversalIDs(t *testing.T) {
	dir := t.TempDir()
	store, err := agent.NewDiskSessionStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../escape", "a/b", `..\escape`, "c:x"} {
		if err := store.Save(&agent.SessionState{SessionID: id}); err == nil {
			t.Errorf("Save(%q) succeeded", id)
		}
		if err := store.Delete(id); err == nil {
			t.Errorf("Delete(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "escape.json")); err == nil {
		t.Fatal("wrote outside the sessions folder")
	}
	if err := store.Save(&agent.SessionState{SessionID: "session-main"}); err != nil {
		t.Fatalf("ordinary id rejected: %v", err)
	}
}
