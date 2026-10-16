package normalizer_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"yoakeland/internal/normalizer"
)

func TestNormalizer_PathContainmentAndSymlinkEscape(t *testing.T) {
	tempDir := t.TempDir()

	// Structure:
	// tempDir/
	//   project/
	//     inside.txt
	//     sub/
	//   outside/
	//     secret.txt
	projectRoot := filepath.Join(tempDir, "project")
	outsideDir := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(filepath.Join(projectRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}

	secretFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("classified"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create symlink inside project pointing outside
	symlinkEscape := filepath.Join(projectRoot, "sym_escape")
	if err := os.Symlink(outsideDir, symlinkEscape); err != nil {
		t.Skip("Symlinks not supported on this platform/privilege")
	}

	norm, err := normalizer.New(projectRoot)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	tests := []struct {
		name        string
		input       string
		shouldError bool
	}{
		{"normal inside", "sub/file.txt", false},
		{"nested dot-dot safe", "sub/../inside.txt", false},
		{"direct dot-dot escape", "../outside/secret.txt", true},
		{"deep traversal", "../../../../../../../etc/passwd", true},
		{"symlink traversal escape", "sym_escape/secret.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := norm.ResolveAndValidatePath(tt.input)
			if tt.shouldError && err == nil {
				t.Errorf("expected security error for path %s, but got nil", tt.input)
			}
			if !tt.shouldError && err != nil {
				t.Errorf("expected path %s to be valid, got err: %v", tt.input, err)
			}
		})
	}
}

func TestNormalizer_ParseShellArgv_Adversarial(t *testing.T) {
	norm, _ := normalizer.New(t.TempDir())

	tests := []struct {
		input       string
		wantLen     int
		hasChain    bool
		isDestruct  bool
		expectError bool
	}{
		{
			input:      "ls -la 'my folder/with spaces'",
			wantLen:    3,
			hasChain:   false,
			isDestruct: false,
		},
		{
			input:      "rm -rf /",
			wantLen:    3,
			hasChain:   false,
			isDestruct: true,
		},
		{
			input:      "echo hello; rm -rf .",
			wantLen:    5,
			hasChain:   true,
			isDestruct: true,
		},
		{
			input:      "cat file.txt && touch file2.txt",
			wantLen:    5,
			hasChain:   true,
			isDestruct: true,
		},
		{
			input:      "git reset --hard HEAD",
			wantLen:    4,
			hasChain:   false,
			isDestruct: true,
		},
		{
			input:       "unclosed 'quote command",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			sc, err := norm.ParseShellArgv(tt.input)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(sc.Argv) != tt.wantLen {
				t.Errorf("argv count mismatch: got %d, want %d (%v)", len(sc.Argv), tt.wantLen, sc.Argv)
			}
			if sc.HasChain != tt.hasChain {
				t.Errorf("hasChain mismatch: got %v, want %v", sc.HasChain, tt.hasChain)
			}
			if norm.IsDestructiveBash(sc) != tt.isDestruct {
				t.Errorf("isDestructive mismatch: got %v, want %v", norm.IsDestructiveBash(sc), tt.isDestruct)
			}
		})
	}
}

func TestNormalizer_ParseShellArgv_BackslashInDoubleQuotes(t *testing.T) {
	norm, _ := normalizer.New(t.TempDir())

	tests := []struct {
		input string
		want  []string
	}{
		{`grep -E "\d+\.go" f`, []string{"grep", "-E", `\d+\.go`, "f"}},
		{`echo "a\nb"`, []string{"echo", `a\nb`}},
		{`echo 'a\nb'`, []string{"echo", `a\nb`}},
		{`echo "say \"hi\""`, []string{"echo", `say "hi"`}},
		{`echo "back\\slash"`, []string{"echo", `back\slash`}},
		{`echo "cost \$5"`, []string{"echo", "cost $5"}},
	}

	for _, tt := range tests {
		got, err := norm.ParseShellArgv(tt.input)
		if err != nil {
			t.Fatalf("ParseShellArgv(%q): %v", tt.input, err)
		}
		if !slices.Equal(got.Argv, tt.want) {
			t.Errorf("ParseShellArgv(%q) = %q, want %q", tt.input, got.Argv, tt.want)
		}
	}
}

func TestIsGitExecPath(t *testing.T) {
	root := t.TempDir()
	n, err := normalizer.New(root)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		".git": true, ".git/config": true, ".git/hooks": true, ".git/hooks/pre-commit": true,
		"sub/.git/config": true, ".GIT/Config": true,
		// Submodule and worktree config run as code too, and the rest of .git is not for writing.
		".git/modules/sub/config": true, ".git/worktrees/w/config.worktree": true,
		".git/HEAD": true, ".git/objects/ab": true, ".git/refs/heads/main": true,
		"config": false, ".gitignore": false, "src/hooks/a.go": false, "src/.gitkeep": false,
	} {
		p, err := n.ResolveAndValidatePath(rel)
		if err != nil {
			t.Fatal(err)
		}
		if got := n.IsGitExecPath(p); got != want {
			t.Errorf("IsGitExecPath(%q) = %v, want %v", rel, got, want)
		}
	}
}

// yoakeland's state dir stays unreachable from a project that contains it.
func TestResolve_RefusesHiddenStateDir(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, ".yoake")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	old := normalizer.Hidden
	normalizer.Hidden = []string{state}
	defer func() { normalizer.Hidden = old }()
	n, _ := normalizer.New(root)
	for _, p := range []string{".yoake", ".yoake/server.json", "./x/../.yoake/tls/ca.key", state + "/providers.json"} {
		if _, err := n.ResolveAndValidatePath(p); err == nil {
			t.Errorf("%s reachable", p)
		}
	}
	if _, err := n.ResolveAndValidatePath(".yoakeish"); err != nil {
		t.Errorf("sibling refused: %v", err)
	}
	// A project inside the state dir isn't locked out of itself.
	inner, _ := normalizer.New(state)
	if _, err := inner.ResolveAndValidatePath("a.txt"); err != nil {
		t.Errorf("project inside state dir refused: %v", err)
	}
}
