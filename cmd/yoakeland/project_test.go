package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
)

// TestProjectRegistry_GetIsIdempotentAndIsolated guards the core promise of
// multi-project support: two different roots get two independent
// ProjectContexts (own registry, own stores), and re-fetching the same root
// returns the exact same instance rather than re-initializing it.
func TestProjectRegistry_GetIsIdempotentAndIsolated(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	projects := newTestRegistry(t)

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
	projects := newTestRegistry(t)

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
	projects := newTestRegistry(t)

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
	projects := newTestRegistry(t)

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
	projects := newTestRegistry(t)
	if _, err := projects.Resolve("session-x", "", ""); err == nil {
		t.Fatal("expected an error when neither an explicit path nor a default project is available")
	}
}

// TestProjectRegistry_List guards that only projects actually opened show
// up — this is a live registry, not a filesystem scan.
func TestProjectRegistry_List(t *testing.T) {
	projects := newTestRegistry(t)
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

func TestProjectRegistry_Remove(t *testing.T) {
	projects := newTestRegistry(t)
	root := t.TempDir()
	if _, err := projects.Get(root); err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if got := projects.List(); len(got) != 1 {
		t.Fatalf("List() before Remove = %v, want 1 entry", got)
	}

	if err := projects.Remove(root); err != nil {
		t.Fatalf("Remove(root): %v", err)
	}
	if got := projects.List(); len(got) != 0 {
		t.Fatalf("List() after Remove = %v, want empty", got)
	}

	// Re-opening after removal must work fresh, not error.
	if _, err := projects.Get(root); err != nil {
		t.Fatalf("Get(root) after Remove: %v", err)
	}
}

// TestProjectRegistry_RemoveEvictsBoundSessions guards the orphaned-agent
// bug: removing a project used to leave any session bound to it fully alive
// (in-memory Agent uncancelled, still in sessionProject) even though the
// project no longer showed up in List() — a session that later got a fresh
// ProjectContext for the same root, and the old Agent, if it had a turn in
// flight, kept running unreferenced. Remove must now evict every bound
// session's Agent and clear its binding.
func TestProjectRegistry_RemoveEvictsBoundSessions(t *testing.T) {
	projects := newTestRegistry(t)
	root := t.TempDir()

	pc, err := projects.Resolve("session-1", root, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	agentBefore := pc.Registry.Get(nil, "session-1")

	if err := projects.Remove(root); err != nil {
		t.Fatalf("Remove(root): %v", err)
	}

	// The old ProjectContext's own registry must have evicted the session's
	// Agent — fetching it again constructs a brand new one rather than
	// returning the same (potentially still-running) instance.
	agentAfter := pc.Registry.Get(nil, "session-1")
	if agentAfter == agentBefore {
		t.Fatal("expected Remove to evict the bound session's Agent from its old registry")
	}

	// The binding itself must be cleared too: an unbound session with no
	// explicit path and no default project is an error, not a silent reuse
	// of the removed project.
	if _, err := projects.Resolve("session-1", "", ""); err == nil {
		t.Fatal("expected session-1's binding to be cleared by Remove")
	}
}

func TestProjectRegistry_KeepsStateOutOfTheProject(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()

	reg, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatalf("NewProjectRegistry: %v", err)
	}
	pc, err := reg.Get(root)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("opening a project wrote into it: %v", entries)
	}
	meta, err := os.ReadFile(filepath.Join(projectstore.DataDir(stateDir, pc.Root), "project.json"))
	if err != nil || !strings.Contains(string(meta), pc.Root) {
		t.Fatalf("data dir should name its project, got %q (%v)", meta, err)
	}
}

// TestProjectRegistry_ListSurvivesRestart guards the actual ownership-model
// fix: the known-projects list is backed by projects.json under the state
// dir, not just this process's in-memory map — a brand new ProjectRegistry
// pointed at the same state dir must see projects opened by a previous one.
func TestProjectRegistry_ListSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()

	first, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatalf("NewProjectRegistry: %v", err)
	}
	if _, err := first.Get(root); err != nil {
		t.Fatalf("Get(root): %v", err)
	}

	second, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatalf("NewProjectRegistry (restart): %v", err)
	}
	got := second.List()
	if len(got) != 1 || got[0] != mustAbs(t, root) {
		t.Fatalf("List() after restart = %v, want [%q]", got, root)
	}
}

// TestProjectKey_ResolvesSymlinksLikeNormalizer guards the fix for the
// symlink bug: projectKey must canonicalize a path exactly like
// normalizer.New does (both call normalizer.CanonicalRoot), so a symlinked
// project path and its real-path equivalent always collapse to the same
// registry key instead of building two independent ProjectContexts that
// both write to different data dirs for the same project.
func TestProjectKey_ResolvesSymlinksLikeNormalizer(t *testing.T) {
	realDir := t.TempDir()
	parent := t.TempDir()
	symlinkPath := filepath.Join(parent, "link")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Skipf("symlinks unsupported in this environment: %v", err)
	}

	keyReal, err := projectKey(realDir)
	if err != nil {
		t.Fatalf("projectKey(realDir): %v", err)
	}
	keySymlink, err := projectKey(symlinkPath)
	if err != nil {
		t.Fatalf("projectKey(symlinkPath): %v", err)
	}
	if keyReal != keySymlink {
		t.Fatalf("projectKey(real) = %q, projectKey(symlink) = %q, want equal", keyReal, keySymlink)
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

// newTestRegistry builds a ProjectRegistry backed by a scratch -state-dir, so
// each test's projects.json is isolated from every other test's.
func newTestRegistry(t *testing.T) *ProjectRegistry {
	t.Helper()
	projects, err := NewProjectRegistry(nil, "", 0, t.TempDir())
	if err != nil {
		t.Fatalf("NewProjectRegistry: %v", err)
	}
	return projects
}

// A TCP peer can use projects the server already has, never a new path.
func TestScopeRemoteProject(t *testing.T) {
	reg, err := NewProjectRegistry(nil, "", 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	known, other := t.TempDir(), t.TempDir()
	if _, err := reg.Get(known); err != nil {
		t.Fatal(err)
	}
	deps := connDeps{projects: reg}
	env := func(typ protocol.EventType, data string) *protocol.Envelope {
		return &protocol.Envelope{Type: typ, Data: json.RawMessage(data)}
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }

	ok := env(protocol.EventReadFile, `{"projectPath":`+q(known+"/./")+`,"path":"a"}`)
	if err := scopeRemoteProject(deps, ok); err != nil {
		t.Fatalf("known project refused: %v", err)
	}
	var d protocol.FileRequestData
	_ = json.Unmarshal(ok.Data, &d)
	if want, _ := projectKey(known); d.ProjectPath != want {
		t.Fatalf("path not canonicalized: %q", d.ProjectPath)
	}
	for _, e := range []*protocol.Envelope{
		env(protocol.EventReadFile, `{"projectPath":`+q(other)+`}`),
		env(protocol.EventReadFile, `{"projectPath":`+q(known)+`,"PROJECTPATH":`+q(other)+`}`),
		env(protocol.EventOpenProject, `{"path":`+q(other)+`}`),
	} {
		if err := scopeRemoteProject(deps, e); err == nil {
			t.Errorf("unknown project allowed: %s", e.Data)
		}
	}
}

// set_web_tools_enabled names a project but no session. It must not bind the empty session id,
// or the next project named the same way is refused as a conflict.
func TestResolve_NoSessionDoesNotBind(t *testing.T) {
	deps := newTestDeps(t)
	if _, err := deps.projects.Resolve("", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.projects.Resolve("", t.TempDir(), ""); err != nil {
		t.Fatalf("second project refused: %v", err)
	}
}
