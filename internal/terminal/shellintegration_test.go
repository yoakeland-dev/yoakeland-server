package terminal

// Internal (white-box) test file, unlike terminal_test.go/
// terminal_unix_test.go — exercises applyShellIntegration/integrationDir
// directly rather than through Manager, since there's no other way to
// observe the argv/env shape spawn() builds without actually spawning a
// real shell for each case.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syntaxCheck shells out to <shell> -n <path>, the standard "parse but
// don't run" check both bash and zsh support — proof the embedded scripts
// (internal/terminal/scripts/*) are at least syntactically valid, since a
// typo there would otherwise only surface as a silently broken interactive
// shell for a real user. Skipped if the shell itself isn't installed on the
// machine running the tests, same guard terminal_unix_test.go uses for
// platform-specific coverage.
func syntaxCheck(t *testing.T, shell, path string) {
	t.Helper()
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("%s not installed, skipping", shell)
	}
	out, err := exec.Command(shell, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("%s -n %s: %v\n%s", shell, path, err, out)
	}
}

func TestShellIntegrationScripts_SyntaxCheck(t *testing.T) {
	dir, err := integrationDir()
	if err != nil {
		t.Fatalf("integrationDir: %v", err)
	}
	syntaxCheck(t, "bash", dir+"/integration.bash")
	syntaxCheck(t, "zsh", dir+"/integration.zshenv")
	syntaxCheck(t, "zsh", dir+"/integration.zshrc")
}

// TestApplyShellIntegration_Bash guards the argv shape spawn() hands to
// sandbox.SpawnPTY: bash gets --rcfile pointed at our script, appended (not
// replacing) whatever argv already had, and the shell path itself (argv[0])
// is untouched — spawn() derives the tab's label from it after this call.
func TestApplyShellIntegration_Bash(t *testing.T) {
	argv, env := applyShellIntegration([]string{"/bin/bash"}, []string{"FOO=bar"}, "n0nce")
	if argv[0] != "/bin/bash" {
		t.Fatalf("expected argv[0] untouched, got %q", argv[0])
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--rcfile") || !strings.Contains(joined, "integration.bash") {
		t.Fatalf("expected --rcfile integration.bash in argv, got %v", argv)
	}
	if len(env) != 2 || env[0] != "FOO=bar" || env[1] != "YOAKE_MARK_NONCE=n0nce" {
		t.Fatalf("expected bash's env plus the mark nonce, got %v", env)
	}
}

// TestApplyShellIntegration_Zsh guards the ZDOTDIR redirect: our dir goes
// in as ZDOTDIR, and the real one (however it started, including empty for
// "unset") is preserved separately so integration.zshrc can chain back to
// the user's actual config.
func TestApplyShellIntegration_Zsh(t *testing.T) {
	argv, env := applyShellIntegration([]string{"/usr/bin/zsh"}, nil, "n0nce")
	if len(argv) != 1 {
		t.Fatalf("expected zsh's argv untouched (integration goes through env, not --rcfile), got %v", argv)
	}
	var sawZdotdir bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "ZDOTDIR=") && strings.Contains(kv, "yoakeland-shell-integration") {
			sawZdotdir = true
		}
	}
	if !sawZdotdir {
		t.Fatalf("expected ZDOTDIR pointed at our script dir, got %v", env)
	}
}

// TestApplyShellIntegration_OtherShellUnmodified guards the explicit scope
// cut (SPEC.md/plan): sh, fish, pwsh, cmd — anything whose basename isn't
// "bash" or "zsh" — must come back byte-for-byte identical, not silently
// half-modified.
func TestApplyShellIntegration_OtherShellUnmodified(t *testing.T) {
	argv, env := applyShellIntegration([]string{"/bin/sh"}, []string{"FOO=bar"}, "n0nce")
	if len(argv) != 1 || argv[0] != "/bin/sh" {
		t.Fatalf("expected /bin/sh's argv untouched, got %v", argv)
	}
	if len(env) != 1 || env[0] != "FOO=bar" {
		t.Fatalf("expected /bin/sh's env untouched, got %v", env)
	}
}

// TestShellIntegration_DoesNotSourceProjectRC: a terminal's HOME is the
// project root, so a repo's own .bashrc / .zshenv / .zshrc must not run when
// a shell starts. Each shell here plants a marker in the project if its rc
// file runs; none may appear.
func TestShellIntegration_DoesNotSourceProjectRC(t *testing.T) {
	dir, err := integrationDir()
	if err != nil {
		t.Fatalf("integrationDir: %v", err)
	}
	project := t.TempDir()
	marker := project + "/ran"
	rc := "echo ran > " + marker + "\n"
	for _, name := range []string{".bashrc", ".zshenv", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(rc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"HOME=" + project, "PATH=" + os.Getenv("PATH")}

	if _, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command("bash", "--rcfile", dir+"/integration.bash", "-i")
		cmd.Env, cmd.Dir = env, project
		_ = cmd.Run()
	}
	if _, err := exec.LookPath("zsh"); err == nil {
		cmd := exec.Command("zsh", "-i")
		cmd.Env = append(append([]string{}, env...), "ZDOTDIR="+dir, "YOAKE_SHELL_INTEGRATION=")
		cmd.Dir = project
		_ = cmd.Run()
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a project's own shell rc file ran in the terminal")
	}
}

// TestScanMarks_IgnoresUnstampedMarks guards the mark nonce: a command or a
// log that prints C and D marks without this shell's nonce must not look
// like a finished command, while the stamped ones still count.
func TestScanMarks_IgnoresUnstampedMarks(t *testing.T) {
	tm := &Terminal{markNonce: "n0nce"}
	tm.scanMarks([]byte("\x1b]633;C\x07\x1b]633;D;0\x07\x1b]633;A\x07"))
	if tm.busy || tm.finished != 0 || tm.ready || tm.lastExit != nil {
		t.Fatalf("unstamped marks changed state: busy=%v finished=%d ready=%v lastExit=%v", tm.busy, tm.finished, tm.ready, tm.lastExit)
	}

	tm.scanMarks([]byte("\x1b]633;A;n0nce\x07\x1b]633;C;n0nce\x07"))
	if !tm.ready || !tm.busy {
		t.Fatalf("stamped A and C not counted: ready=%v busy=%v", tm.ready, tm.busy)
	}
	tm.scanMarks([]byte("\x1b]633;D;3;n0nce\x07"))
	if tm.busy || tm.finished != 1 || tm.lastExit == nil || *tm.lastExit != 3 {
		t.Fatalf("stamped D not counted: busy=%v finished=%d lastExit=%v", tm.busy, tm.finished, tm.lastExit)
	}
}
