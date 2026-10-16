package permission_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
)

func newEngine(t *testing.T) (*permission.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	norm, err := normalizer.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return permission.New(norm, nil), norm.ProjectRoot
}

func mustAdd(t *testing.T, e *permission.Engine, scope permission.Scope, r permission.Rule) permission.Rule {
	t.Helper()
	got, err := e.AddRule(scope, r)
	if err != nil {
		t.Fatalf("AddRule(%+v): %v", r, err)
	}
	return got
}

func bashRule(d permission.Decision, pat ...string) permission.Rule {
	return permission.Rule{Tool: permission.ToolBash, Pattern: pat, Decision: d}
}

func check(e *permission.Engine, tool, target, mode string) permission.Decision {
	return e.Check(permission.Request{Tool: tool, Target: target, Mode: mode}).Decision
}

func TestDefaultsAsk(t *testing.T) {
	e, _ := newEngine(t)
	if got := check(e, "file_read", "x", "read_write"); got != permission.DecisionAllow {
		t.Errorf("file_read = %s", got)
	}
	for _, c := range []struct{ tool, target string }{
		{"bash", "ls"}, {"bash", "git status"}, {"file_write", "a.go"}, {"web_fetch", "example.com"}, {"terminal_start", "npm run dev"},
	} {
		if got := check(e, c.tool, c.target, "read_write"); got != permission.DecisionAsk {
			t.Errorf("%s %q = %s, want ask", c.tool, c.target, got)
		}
	}
	// A command the tokenizer rejects is refused outright.
	if got := check(e, "bash", "echo 'unclosed", "read_write"); got != permission.DecisionDeny {
		t.Errorf("unparseable bash = %s, want deny", got)
	}
}

// Deny beats ask beats allow, whatever scope each rule lives in.
func TestPrecedenceAcrossScopes(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	e := permission.New(norm, permission.NewPersistentStore(dir))
	e.SetGlobal(permission.OpenGlobal(t.TempDir()))

	mustAdd(t, e, permission.ScopeSession, bashRule(permission.DecisionAllow, "go", "test", "*"))
	if got := check(e, "bash", "go test ./...", "read_write"); got != permission.DecisionAllow {
		t.Fatalf("allow = %s", got)
	}
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAsk, "go", "test", "*"))
	if got := check(e, "bash", "go test ./...", "read_write"); got != permission.DecisionAsk {
		t.Fatalf("ask over allow = %s", got)
	}
	mustAdd(t, e, permission.ScopeGlobal, bashRule(permission.DecisionDeny, "go", "test", "./secret/..."))
	if got := check(e, "bash", "go test ./...", "read_write"); got != permission.DecisionAsk {
		t.Fatalf("non-matching deny changed the result: %s", got)
	}
	if got := check(e, "bash", "go test ./secret/...", "read_write"); got != permission.DecisionDeny {
		t.Fatalf("global deny over session allow = %s", got)
	}
}

func TestBashAllowMatchesArgvTokens(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAllow, "go", "test", "*"))
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAllow, "make", "lint"))
	for target, want := range map[string]permission.Decision{
		"go test":                   permission.DecisionAllow,
		"go test ./... -run 'A B'":  permission.DecisionAllow,
		"go  test   -v":             permission.DecisionAllow,
		"go testing":                permission.DecisionAsk, // token, not string, prefix
		"gofmt test":                permission.DecisionAsk,
		"go":                        permission.DecisionAsk,
		"/usr/bin/go test":          permission.DecisionAsk, // allow needs the program as written
		"FOO=1 go test":             permission.DecisionAsk,
		"go test ./... && rm -rf x": permission.DecisionAsk, // chained: never an allow
		"go test ./...; ls":         permission.DecisionAsk,
		"go test $(whoami)":         permission.DecisionAsk,
		"go test `id`":              permission.DecisionAsk,
		"go test | tee":             permission.DecisionAsk,
		"make lint":                 permission.DecisionAllow,
		"make lint extra":           permission.DecisionAsk, // no wildcard: exact argv
		"make":                      permission.DecisionAsk,
		"go test 'unclosed":         permission.DecisionDeny,
	} {
		if got := check(e, "bash", target, "read_write"); got != want {
			t.Errorf("bash %q = %s, want %s", target, got, want)
		}
	}
	// A terminal is a real shell: an allow rule for bash never covers it.
	if got := check(e, "terminal_start", "go test ./...", "read_write"); got != permission.DecisionAsk {
		t.Errorf("terminal_start = %s, want ask", got)
	}
	if got := check(e, "terminal_output", "go test ./...\n", "read_write"); got != permission.DecisionAsk {
		t.Errorf("terminal_output = %s, want ask", got)
	}
}

// Deny rules must survive the usual ways of spelling the same command.
func TestDenyIsBasenameAndWrapperAware(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionDeny, "rm", "*"))
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionDeny, "git", "push", "*"))
	for _, cmd := range []string{
		"rm -rf x", "/bin/rm -rf x", "./rm x", "env FOO=1 rm x", "nice -n 5 rm x", "timeout 5 rm x", "sudo rm x",
		"sh -c 'rm x'", "bash -lc \"echo hi; rm x\"", "echo a && rm x", "echo $(rm x)", "echo `rm x`",
		"find . -exec rm {} ;", "xargs rm", "git push origin main", "git -C . push", "git -c push.default=current push",
		"git --git-dir=.git push", "/usr/bin/git push", "eval 'rm x'", "echo a | xargs -n1 rm",
	} {
		for _, tool := range []string{"bash", "terminal_start"} {
			if got := check(e, tool, cmd, "read_write"); got != permission.DecisionDeny {
				t.Errorf("%s %q = %s, want deny", tool, cmd, got)
			}
		}
	}
	for _, cmd := range []string{"echo rm", "git pull", "git status", "ls -la", "rmdir x"} {
		if got := check(e, "bash", cmd, "read_write"); got == permission.DecisionDeny {
			t.Errorf("bash %q wrongly denied", cmd)
		}
	}
}

func TestUnsafeAllowRulesRejected(t *testing.T) {
	e, _ := newEngine(t)
	for _, pat := range [][]string{
		{"*"}, {"sh", "*"}, {"bash", "-c", "*"}, {"env", "*"}, {"xargs", "*"}, {"find", "*"}, {"sudo", "*"}, {"python3", "*"},
		{"node", "*"}, {"git", "*"}, {"git", "-c", "*"}, {"git", "config", "*"}, {"npm", "exec", "*"}, {"npm", "run", "*"},
		{"docker", "run", "*"}, {"rm", "*"}, {"curl", "*"}, {"go", "-exec", "x", "*"}, {"go", "*"}, {"ssh", "*"}, {"a", "*", "b"},
	} {
		if _, err := e.AddRule(permission.ScopeProject, bashRule(permission.DecisionAllow, pat...)); err == nil {
			t.Errorf("allow %v was accepted", pat)
		}
	}
	// The same shapes are fine to deny or ask about, and exact commands are fine to allow.
	for _, r := range []permission.Rule{
		bashRule(permission.DecisionDeny, "sh", "*"), bashRule(permission.DecisionAsk, "*"),
		bashRule(permission.DecisionAllow, "rm", "-rf", "build"), bashRule(permission.DecisionAllow, "npm", "run", "test", "*"),
		bashRule(permission.DecisionAllow, "git", "status", "*"),
	} {
		if _, err := e.AddRule(permission.ScopeProject, r); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func fileRule(d permission.Decision, tool, pat string) permission.Rule {
	return permission.Rule{Tool: tool, Pattern: []string{pat}, Decision: d}
}

func TestPathRules(t *testing.T) {
	e, root := newEngine(t)
	for _, d := range []string{"internal/agent", "internal/agentic", ".git/hooks", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "internal", "link")); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, "internal/agent/**"))
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, "docs/*.md"))
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, ".env"))
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, ".git/hooks/**"))
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, "internal/link/**"))

	for _, c := range []struct {
		tool, path string
		want       permission.Decision
	}{
		{"file_write", "internal/agent/a.go", permission.DecisionAllow},
		{"file_edit", "internal/agent/deep/er/a.go", permission.DecisionAllow},
		{"file_write", "./internal/agent/a.go", permission.DecisionAllow},
		{"file_write", filepath.Join(root, "internal/agent/a.go"), permission.DecisionAllow},
		{"file_write", "internal/agent", permission.DecisionAllow}, // the directory itself
		{"file_write", "internal/agentic/a.go", permission.DecisionAsk},
		{"file_write", "internal/agent/../agentic/a.go", permission.DecisionAsk},
		{"file_write", "internal/other.go", permission.DecisionAsk},
		{"file_write", "docs/a.md", permission.DecisionAllow},
		{"file_write", "docs/sub/a.md", permission.DecisionAsk}, // * stays in one segment
		{"file_write", "docs/a.txt", permission.DecisionAsk},
		{"file_write", ".env", permission.DecisionAsk}, // never allowed by a rule
		{"file_write", ".git/hooks/pre-commit", permission.DecisionAsk},
		{"file_write", "../escape.go", permission.DecisionAsk},
		{"file_write", "internal/link/x.go", permission.DecisionAsk}, // symlink out of the root
	} {
		if got := check(e, c.tool, c.path, "read_write"); got != c.want {
			t.Errorf("%s %q = %s, want %s", c.tool, c.path, got, c.want)
		}
	}

	// A deny needs only one of the spellings to match, and ignores case.
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionDeny, permission.ToolEdit, "internal/agent/secret.go"))
	mustAdd(t, e, permission.ScopeProject, fileRule(permission.DecisionDeny, permission.ToolEdit, ".env*"))
	for _, p := range []string{"internal/agent/secret.go", "internal/agent/x/../secret.go", "INTERNAL/agent/SECRET.go", ".env", ".ENV.local", filepath.Join(root, ".env.prod")} {
		if got := check(e, "file_edit", p, "read_write"); got != permission.DecisionDeny {
			t.Errorf("file_edit %q = %s, want deny", p, got)
		}
	}
}

func TestPathPatternValidation(t *testing.T) {
	e, _ := newEngine(t)
	for _, p := range []string{"**", "*", ".", "/etc/passwd", "../x", "a/../b", "a/**/b", "[bad"} {
		if _, err := e.AddRule(permission.ScopeProject, fileRule(permission.DecisionAllow, permission.ToolEdit, p)); err == nil {
			t.Errorf("allow path %q accepted", p)
		}
	}
	if _, err := e.AddRule(permission.ScopeGlobal, fileRule(permission.DecisionAllow, permission.ToolEdit, "docs/**")); err == nil {
		t.Error("a global edit allow was accepted")
	}
	if _, err := e.AddRule(permission.ScopeProject, fileRule(permission.DecisionDeny, permission.ToolEdit, "**")); err != nil {
		t.Errorf("deny ** rejected: %v", err)
	}
}

// Allow rules follow the ratchet: granted in a mode, in force from there up,
// never in read_only or plan.
func TestModeRatchet(t *testing.T) {
	e, _ := newEngine(t)
	sug := permission.Suggestion{ID: "prefix", Tool: permission.ToolBash, Pattern: []string{"go", "test", "*"}, Scope: permission.ScopeProject}
	if _, err := e.Remember(sug, true, "", "accept_edits"); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string]permission.Decision{
		"read_only": permission.DecisionAsk, "plan": permission.DecisionAsk, "read_write": permission.DecisionAsk,
		"accept_edits": permission.DecisionAllow, "auto": permission.DecisionAllow,
	} {
		if got := check(e, "bash", "go test ./...", mode); got != want {
			t.Errorf("mode %s = %s, want %s", mode, got, want)
		}
	}
	// A rule with no modes is in force in every asking mode, but still not read_only/plan.
	e2, _ := newEngine(t)
	mustAdd(t, e2, permission.ScopeProject, bashRule(permission.DecisionAllow, "go", "vet", "*"))
	for mode, want := range map[string]permission.Decision{
		"read_only": permission.DecisionAsk, "plan": permission.DecisionAsk, "read_write": permission.DecisionAllow, "auto": permission.DecisionAllow,
	} {
		if got := check(e2, "bash", "go vet ./...", mode); got != want {
			t.Errorf("unrestricted rule, mode %s = %s, want %s", mode, got, want)
		}
	}
	// Deny applies in every mode.
	mustAdd(t, e2, permission.ScopeProject, bashRule(permission.DecisionDeny, "go", "vet", "./bad"))
	if got := check(e2, "bash", "go vet ./bad", "plan"); got != permission.DecisionDeny {
		t.Errorf("deny in plan = %s", got)
	}
}

func TestSuggestions(t *testing.T) {
	e, root := newEngine(t)
	ids := func(r permission.Result) string {
		var out []string
		for _, s := range r.Suggestions {
			out = append(out, s.ID+"="+strings.Join(s.Pattern, " "))
		}
		return strings.Join(out, " | ")
	}
	for cmd, want := range map[string]string{
		"go test ./...":          "prefix=go test * | exact=go test ./...",
		"npm run test -- --ci":   "prefix=npm run test * | exact=npm run test -- --ci",
		"ls -la src":             "prefix=ls * | exact=ls -la src",
		"git status":             "prefix=git status * | exact=git status",
		"git -C x status":        "exact=git -C x status",        // no wildcard past an option
		"git config user.name x": "exact=git config user.name x", // git config can run programs
		"npm exec foo":           "exact=npm exec foo",
		"bash -c 'ls'":           "exact=bash -c ls",
		"python3 x.py":           "exact=python3 x.py",
		"env FOO=1 go test":      "exact=env FOO=1 go test",
		"docker run alpine":      "exact=docker run alpine",
		"./run.sh --now":         "exact=./run.sh --now",
		"ls *":                   "prefix=ls *", // a literal * can't be an exact rule
		"rm -rf build":           "",            // destructive: nothing to remember
		"git reset --hard":       "",
		"ls; rm x":               "", // chained
		"echo $HOME":             "",
	} {
		if got := ids(e.Check(permission.Request{Tool: "bash", Target: cmd, Mode: "read_write"})); got != want {
			t.Errorf("suggestions for %q = %q, want %q", cmd, got, want)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "internal/agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"internal/agent/a.go": "dir=internal/agent/** | exact=internal/agent/a.go",
		"main.go":             "exact=main.go",
		"new/dir/x.go":        "dir=new/dir/** | exact=new/dir/x.go",
		".env":                "",
		".git/config":         "",
		"../out.go":           "",
	} {
		if got := ids(e.Check(permission.Request{Tool: "file_write", Target: path, Mode: "read_write"})); got != want {
			t.Errorf("suggestions for %q = %q, want %q", path, got, want)
		}
	}
	if got := ids(e.Check(permission.Request{Tool: "terminal_start", Target: "npm run dev"})); got != "" {
		t.Errorf("terminal_start offered %q", got)
	}
	// A picked suggestion round-trips into a rule that then allows the call.
	r := e.Check(permission.Request{Tool: "bash", Target: "go build ./cmd/x", Mode: "read_write"})
	if _, err := e.Remember(r.Suggestions[0], true, permission.ScopeSession, "read_write"); err != nil {
		t.Fatal(err)
	}
	if got := check(e, "bash", "go build ./...", "read_write"); got != permission.DecisionAllow {
		t.Errorf("after remembering the prefix: %s", got)
	}
	// Denying with a suggestion stores a deny rule.
	if _, err := e.Remember(e.Check(permission.Request{Tool: "web_fetch", Target: "evil.example"}).Suggestions[0], false, "", "read_write"); err != nil {
		t.Fatal(err)
	}
	if got := check(e, "web_fetch", "evil.example", "read_write"); got != permission.DecisionDeny {
		t.Errorf("remembered deny = %s", got)
	}
}

func TestExamplesCheckedOnAdd(t *testing.T) {
	e, _ := newEngine(t)
	ok := bashRule(permission.DecisionAllow, "go", "test", "*")
	ok.Match, ok.NotMatch = []string{"go test ./..."}, []string{"go build", "gofmt"}
	if _, err := e.AddRule(permission.ScopeProject, ok); err != nil {
		t.Fatalf("good examples rejected: %v", err)
	}
	bad := bashRule(permission.DecisionAllow, "go", "vet", "*")
	bad.NotMatch = []string{"go vet ./..."}
	if _, err := e.AddRule(permission.ScopeProject, bad); err == nil {
		t.Error("a rule matching its own notMatch example was accepted")
	}
	bad2 := bashRule(permission.DecisionAllow, "go", "vet", "*")
	bad2.Match = []string{"go build"}
	if _, err := e.AddRule(permission.ScopeProject, bad2); err == nil {
		t.Error("a rule missing its match example was accepted")
	}
}

func TestRemoveAndDuplicates(t *testing.T) {
	e, _ := newEngine(t)
	a := mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAllow, "go", "vet", "*"))
	b := mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAllow, "go", "vet", "*"))
	if a.ID != b.ID {
		t.Errorf("duplicate rule got a second id")
	}
	if n := len(e.Rules()); n != 1 {
		t.Fatalf("%d rules", n)
	}
	if _, err := e.RemoveRule(a.ID); err != nil {
		t.Fatal(err)
	}
	if got := check(e, "bash", "go vet ./...", "read_write"); got != permission.DecisionAsk {
		t.Errorf("after revoke: %s", got)
	}
	if _, err := e.RemoveRule(a.ID); err == nil {
		t.Error("removing twice succeeded")
	}
}

func TestPresets(t *testing.T) {
	e, _ := newEngine(t)
	if got := check(e, "bash", "git status", "read_write"); got != permission.DecisionAsk {
		t.Fatalf("presets are off by default, got %s", got)
	}
	e.SetPresets(true)
	for cmd, want := range map[string]permission.Decision{
		"ls": permission.DecisionAllow, "ls -la src": permission.DecisionAllow, "pwd": permission.DecisionAllow,
		"git status": permission.DecisionAllow, "git diff HEAD~1": permission.DecisionAllow, "git log --oneline -5": permission.DecisionAllow,
		"git show HEAD":                             permission.DecisionAllow,
		"git diff --output=x":                       permission.DecisionAsk,
		"git diff --ext-diff":                       permission.DecisionAsk,
		"git diff --no-index /etc/passwd /dev/null": permission.DecisionAsk,
		"git log -c core.pager=x":                   permission.DecisionAsk,
		"git -c core.pager=x log":                   permission.DecisionAsk,
		"git -C /elsewhere status":                  permission.DecisionAsk,
		"git commit -m x":                           permission.DecisionAsk,
		"ls; rm x":                                  permission.DecisionAsk,
		"git push":                                  permission.DecisionAsk,
	} {
		if got := check(e, "bash", cmd, "read_write"); got != want {
			t.Errorf("preset: bash %q = %s, want %s", cmd, got, want)
		}
	}
	if got := check(e, "bash", "git status", "plan"); got != permission.DecisionAsk {
		t.Errorf("preset in plan = %s", got)
	}
	// A user can override a preset with an ask or deny rule, but not delete it.
	mustAdd(t, e, permission.ScopeProject, bashRule(permission.DecisionAsk, "git", "status", "*"))
	if got := check(e, "bash", "git status", "read_write"); got != permission.DecisionAsk {
		t.Errorf("ask over preset = %s", got)
	}
	if _, err := e.RemoveRule("preset:ls"); err == nil {
		t.Error("removed a preset")
	}
}

func TestEngineWithoutGlobalStillWorks(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.AddRule(permission.ScopeGlobal, bashRule(permission.DecisionDeny, "rm", "*")); err == nil {
		t.Error("global add with no global store succeeded")
	}
}

// Reads need no allow rule. A read rule can deny or ask, and secret-shaped
// files ask on their own; ordinary files are allowed.
func TestReadRules(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, permission.Rule{Tool: permission.ToolRead, Pattern: []string{"private/**"}, Decision: permission.DecisionDeny})
	mustAdd(t, e, permission.ScopeProject, permission.Rule{Tool: permission.ToolRead, Pattern: []string{"docs/*.md"}, Decision: permission.DecisionAsk})
	for _, c := range []struct {
		tool, target string
		want         permission.Decision
	}{
		{"file_read", "src/a.go", permission.DecisionAllow},
		{"file_read", "private/key.txt", permission.DecisionDeny},
		{"search", "private", permission.DecisionDeny},
		{"search", ".", permission.DecisionAllow},
		{"file_read", "docs/x.md", permission.DecisionAsk},
		{"file_read", ".env", permission.DecisionAsk},
		{"file_read", "config/.ENV.local", permission.DecisionAsk},
		{"file_read", "ssh/id_rsa.pub", permission.DecisionAsk},
		{"file_read", "sub/.npmrc", permission.DecisionAsk},
		{"file_read", "sub/.git/config", permission.DecisionAsk},
		{"file_read", ".git/HEAD", permission.DecisionAllow},
	} {
		if got := check(e, c.tool, c.target, "read_write"); got != c.want {
			t.Errorf("%s %q = %s, want %s", c.tool, c.target, got, c.want)
		}
	}
	if _, err := e.AddRule(permission.ScopeProject, permission.Rule{Tool: permission.ToolRead, Pattern: []string{"src/**"}, Decision: permission.DecisionAllow}); err == nil {
		t.Error("an allow read rule was accepted")
	}
}
