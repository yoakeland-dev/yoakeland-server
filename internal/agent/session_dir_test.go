package agent_test

import (
	"os"
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
