package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProjectRegistry_GetIsIdempotentAndIsolated guards the core promise of
// multi-project support: two different roots get two independent
// ProjectContexts (own registry, own stores), and re-fetching the same root
// returns the exact same instance rather than re-initializing it.
func TestProjectRegistry_GetIsIdempotentAndIsolated(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	projects := NewProjectRegistry(nil, 0)

	pcA1, err := projects.Get(rootA)
	if err != nil {
		t.Fatalf("Get(rootA): %v", err)
	}
	pcA2, err := projects.Get(rootA)
	if err != nil {
		t.Fatalf("Get(rootA) again: %v", err)
	}
	if pcA1 != pcA2 {
		t.Fatal("expected repeated Get on the same root to return the same ProjectContext")
	}

	pcB, err := projects.Get(rootB)
	if err != nil {
		t.Fatalf("Get(rootB): %v", err)
	}
	if pcA1 == pcB {
		t.Fatal("expected different roots to get different ProjectContexts")
	}
	if pcA1.Registry == pcB.Registry {
		t.Fatal("expected different projects to have independent session registries")
	}
}

// TestProjectRegistry_ResolveBindsSessionOnFirstUse guards the session <->
// project binding: an explicit path on first use sticks, later calls for
// the same session don't need to repeat it, and a conflicting path for an
// already-bound session is rejected rather than silently switched.
func TestProjectRegistry_ResolveBindsSessionOnFirstUse(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	projects := NewProjectRegistry(nil, 0)

	pc, err := projects.Resolve("session-1", rootA, "")
	if err != nil {
		t.Fatalf("Resolve with explicit path: %v", err)
	}
	if pc.Root != mustAbs(t, rootA) {
		t.Fatalf("pc.Root = %q, want %q", pc.Root, rootA)
	}

	// No explicit path this time: must fall back to the existing binding.
	pcAgain, err := projects.Resolve("session-1", "", "")
	if err != nil {
		t.Fatalf("Resolve falling back to existing binding: %v", err)
	}
	if pcAgain != pc {
		t.Fatal("expected the session's existing project binding to be reused")
	}

	// A different, conflicting path for the same session must be rejected.
	if _, err := projects.Resolve("session-1", rootB, ""); err == nil {
		t.Fatal("expected an error rebinding an already-bound session to a different project")
	}
}

// TestProjectRegistry_ResolveFallsBackToDefault guards the common
// single-project case: a session that never specifies a projectPath still
// works, using the server's default project.
func TestProjectRegistry_ResolveFallsBackToDefault(t *testing.T) {
	defaultRoot := t.TempDir()
	projects := NewProjectRegistry(nil, 0)

	pc, err := projects.Resolve("session-no-path", "", defaultRoot)
	if err != nil {
		t.Fatalf("Resolve with default fallback: %v", err)
	}
	if pc.Root != mustAbs(t, defaultRoot) {
		t.Fatalf("pc.Root = %q, want the default project %q", pc.Root, defaultRoot)
	}
}

// TestProjectRegistry_DefaultFallbackDoesNotLockSession guards the bug where
// a session that only ever touched the server's default project (e.g. via a
// stray event with no projectPath, like a permission response) would get
// permanently bound to it — making that server's default project the only
// one the session could ever act in.
func TestProjectRegistry_DefaultFallbackDoesNotLockSession(t *testing.T) {
	defaultRoot := t.TempDir()
	otherRoot := t.TempDir()
	projects := NewProjectRegistry(nil, 0)

	if _, err := projects.Resolve("session-shared", "", defaultRoot); err != nil {
		t.Fatalf("Resolve with default fallback: %v", err)
	}

	pc, err := projects.Resolve("session-shared", otherRoot, defaultRoot)
	if err != nil {
		t.Fatalf("Resolve with explicit different project after default fallback: %v", err)
	}
	if pc.Root != mustAbs(t, otherRoot) {
		t.Fatalf("pc.Root = %q, want the explicit project %q", pc.Root, otherRoot)
	}
}

// TestProjectRegistry_ResolveErrorsWithNoPathAndNoDefault guards the
// explicit multi-tenant case: if the server has no default project and a
// session never named one, that's a real error, not a silent guess.
func TestProjectRegistry_ResolveErrorsWithNoPathAndNoDefault(t *testing.T) {
	projects := NewProjectRegistry(nil, 0)
	if _, err := projects.Resolve("session-x", "", ""); err == nil {
		t.Fatal("expected an error when neither an explicit path nor a default project is available")
	}
}

// TestProjectRegistry_List guards that only projects actually opened show
// up — this is a live registry, not a filesystem scan.
func TestProjectRegistry_List(t *testing.T) {
	projects := NewProjectRegistry(nil, 0)
	if got := projects.List(); len(got) != 0 {
		t.Fatalf("expected an empty list before opening anything, got %v", got)
	}

	rootA := t.TempDir()
	if _, err := projects.Get(rootA); err != nil {
		t.Fatalf("Get(rootA): %v", err)
	}

	got := projects.List()
	if len(got) != 1 || got[0] != mustAbs(t, rootA) {
		t.Fatalf("List() = %v, want [%q]", got, rootA)
	}
}

// TestListProjectFiles guards @file-mention autocomplete's data source:
// every real file shows up relative to the root, ignored dirs (VCS,
// dependency, build) and dot-prefixed entries are skipped, and hidden files
// alongside real ones don't leak through either.
func TestListProjectFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", rel, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", rel, err)
		}
	}

	write("README.md")
	write("src/main.go")
	write(".git/HEAD")
	write(".yoake/usage.json")
	write("node_modules/pkg/index.js")
	write(".hidden")

	got, err := listProjectFiles(root)
	if err != nil {
		t.Fatalf("listProjectFiles: %v", err)
	}

	want := []string{"README.md", "src/main.go"}
	if len(got) != len(want) {
		t.Fatalf("listProjectFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listProjectFiles = %v, want %v", got, want)
		}
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	key, err := projectKey(path)
	if err != nil {
		t.Fatalf("projectKey(%q): %v", path, err)
	}
	return key
}
