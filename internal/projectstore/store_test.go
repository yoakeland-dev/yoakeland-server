package projectstore

import "testing"

// TestStore_UpsertListRemove guards the round-trip this whole package exists
// for: a project recorded via Upsert shows up in List, sorted most-recent
// first, and Remove drops it again — across fresh Store instances pointed at
// the same directory, so it's actually the file that's authoritative, not
// in-memory state.
func TestStore_UpsertListRemove(t *testing.T) {
	dir := t.TempDir()

	s1, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s1.Upsert("/a"); err != nil {
		t.Fatalf("Upsert(/a): %v", err)
	}
	if err := s1.Upsert("/b"); err != nil {
		t.Fatalf("Upsert(/b): %v", err)
	}
	// Re-upserting /a bumps it to most-recently-opened.
	if err := s1.Upsert("/a"); err != nil {
		t.Fatalf("Upsert(/a) again: %v", err)
	}

	s2, err := New(dir)
	if err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	got, err := s2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Path != "/a" || got[1].Path != "/b" {
		t.Fatalf("List() = %+v, want [/a, /b] (most-recent first)", got)
	}

	if err := s2.Remove("/a"); err != nil {
		t.Fatalf("Remove(/a): %v", err)
	}
	// Removing something already gone is not an error.
	if err := s2.Remove("/a"); err != nil {
		t.Fatalf("Remove(/a) again: %v", err)
	}

	got, err = s1.List()
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/b" {
		t.Fatalf("List() after Remove(/a) = %+v, want [/b]", got)
	}
}

// TestStore_ListOnFreshDirIsEmpty guards the no-projects.json-yet case (a
// brand new server install) not being treated as an error.
func TestStore_ListOnFreshDirIsEmpty(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List() on fresh store = %+v, want empty", got)
	}
}
