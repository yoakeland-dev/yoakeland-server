package permission_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
)

func engineAt(t *testing.T, dir string) *permission.Engine {
	t.Helper()
	norm, err := normalizer.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return permission.New(norm, permission.NewPersistentStore(dir))
}

func TestRulesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	e1 := engineAt(t, dir)
	mustAdd(t, e1, permission.ScopeProject, bashRule(permission.DecisionAllow, "go", "test", "*"))
	mustAdd(t, e1, permission.ScopeSession, bashRule(permission.DecisionAllow, "go", "vet", "*"))

	e2 := engineAt(t, dir)
	if got := check(e2, "bash", "go test ./...", "read_write"); got != permission.DecisionAllow {
		t.Errorf("persisted project rule lost: %s", got)
	}
	if got := check(e2, "bash", "go vet ./...", "read_write"); got != permission.DecisionAsk {
		t.Errorf("session rule survived a restart: %s", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "permissions.json"))
	var m struct{ Version int }
	_ = json.Unmarshal(b, &m)
	if m.Version != 2 {
		t.Errorf("version = %d", m.Version)
	}
}

func TestGlobalRulesSharedBetweenProjects(t *testing.T) {
	state := t.TempDir()
	a, b := engineAt(t, t.TempDir()), engineAt(t, t.TempDir())
	a.SetGlobal(permission.OpenGlobal(state))
	b.SetGlobal(permission.OpenGlobal(state))
	mustAdd(t, a, permission.ScopeGlobal, bashRule(permission.DecisionDeny, "terraform", "destroy", "*"))
	if got := check(b, "bash", "terraform destroy -auto-approve", "read_write"); got != permission.DecisionDeny {
		t.Errorf("global deny not seen by the other project: %s", got)
	}
	if _, err := os.Stat(filepath.Join(state, "permissions.global.json")); err != nil {
		t.Errorf("no global file: %v", err)
	}
}

// A v1 file: exact tool:target entries. Migration must keep exactly their
// meaning, keep a backup, and drop what it cannot express safely.
func TestMigrationFromV1(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	root := norm.ProjectRoot
	v1 := `{"version":1,"rules":{
	  "file_write:src/main.go":        {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "file_edit:` + root + `/docs/a.md": {"allowed":true, "updatedAt":"2026-01-02T03:04:05Z"},
	  "file_write:.env":               {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "file_write:[x]/*.go":           {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "file_write:/etc/passwd":        {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "file_write:old/secret.txt":     {"allowed":false, "updatedAt":"2026-01-02T03:04:05Z"},
	  "bash:go test ./...":            {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "bash:ls *":                     {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "bash:ls && rm x":               {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "bash:rm -rf /tmp/x":            {"allowed":false, "updatedAt":"2026-01-02T03:04:05Z"},
	  "terminal_output:rm x\n":        {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"},
	  "web_fetch:example.com":         {"allowed":true,  "updatedAt":"2026-01-02T03:04:05Z"}
	}}`
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	e := engineAt(t, dir)
	errs, notes := e.Problems()
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(notes) < 4 {
		t.Errorf("expected notes about dropped entries, got %v", notes)
	}
	if bak, err := os.ReadFile(path + ".v1.bak"); err != nil || string(bak) != v1 {
		t.Errorf("v1 backup missing or changed: %v", err)
	}
	var m struct{ Version int }
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &m)
	if m.Version != 2 {
		t.Errorf("file not rewritten as v2: %s", b)
	}

	for _, c := range []struct {
		tool, target string
		want         permission.Decision
	}{
		{"file_write", "src/main.go", permission.DecisionAllow},
		{"file_edit", "src/main.go", permission.DecisionAsk}, // v1 keys were per tool
		{"file_write", "src/other.go", permission.DecisionAsk},
		{"file_edit", "docs/a.md", permission.DecisionAllow}, // absolute key became relative
		{"file_write", ".env", permission.DecisionAsk},       // never allowed
		{"file_write", "old/secret.txt", permission.DecisionDeny},
		{"bash", "go test ./...", permission.DecisionAllow},
		{"bash", "go test ./... -v", permission.DecisionAsk}, // exact, not a prefix
		{"bash", "ls x", permission.DecisionAsk},             // the literal-* entry was dropped
		{"bash", "ls && rm x", permission.DecisionAsk},
		{"bash", "rm -rf /tmp/x", permission.DecisionDeny},
		{"terminal_output", "rm x\n", permission.DecisionAsk},
		{"web_fetch", "example.com", permission.DecisionAllow},
	} {
		if got := check(e, c.tool, c.target, "read_write"); got != c.want {
			t.Errorf("%s %q = %s, want %s", c.tool, c.target, got, c.want)
		}
	}
	for _, r := range e.Rules() {
		if r.Source != permission.SourceMigrated {
			t.Errorf("rule %s source = %q", r.ID, r.Source)
		}
	}

	// Reloading the migrated file gives the same rules and leaves the backup alone.
	e2 := engineAt(t, dir)
	if len(e2.Rules()) != len(e.Rules()) {
		t.Errorf("reload changed the rule count: %d vs %d", len(e2.Rules()), len(e.Rules()))
	}
	if bak, _ := os.ReadFile(path + ".v1.bak"); string(bak) != v1 {
		t.Error("backup rewritten")
	}
}

// A file that cannot be trusted must fail closed: nothing is allowed, and the
// file is left for the user to fix rather than overwritten.
func TestCorruptFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not json":        `{"version":2,"rules":[`,
		"future version":  `{"version":3,"rules":[]}`,
		"bad decision":    `{"version":2,"rules":[{"id":"r1","tool":"bash","pattern":["ls"],"decision":"maybe"}]}`,
		"unsafe allow":    `{"version":2,"rules":[{"id":"r1","tool":"bash","pattern":["sh","*"],"decision":"allow"}]}`,
		"no id":           `{"version":2,"rules":[{"tool":"bash","pattern":["ls"],"decision":"deny"}]}`,
		"unknown tool":    `{"version":2,"rules":[{"id":"r1","tool":"nope","pattern":["x"],"decision":"deny"}]}`,
		"failing example": `{"version":2,"rules":[{"id":"r1","tool":"bash","pattern":["ls"],"decision":"deny","match":["pwd"]}]}`,
		"empty file":      ``,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "permissions.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			e := engineAt(t, dir)
			e.SetPresets(true)
			errs, _ := e.Problems()
			if len(errs) != 1 {
				t.Fatalf("problems = %v", errs)
			}
			// Even a session allow and a preset are not honoured while a file is broken.
			if _, err := e.AddRule(permission.ScopeSession, bashRule(permission.DecisionAllow, "go", "test", "*")); err != nil {
				t.Fatalf("session rules should still be addable: %v", err)
			}
			for _, cmd := range []string{"go test ./...", "git status", "ls"} {
				if got := check(e, "bash", cmd, "read_write"); got == permission.DecisionAllow {
					t.Errorf("%q allowed while the rules file is corrupt", cmd)
				}
			}
			if _, err := e.AddRule(permission.ScopeProject, bashRule(permission.DecisionDeny, "rm", "*")); err == nil || !strings.Contains(err.Error(), "could not be loaded") {
				t.Errorf("wrote to a corrupt file: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != content {
				t.Errorf("corrupt file was modified: %q", b)
			}
		})
	}
}
