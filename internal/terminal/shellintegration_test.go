package terminal

// Internal (white-box) test file, unlike terminal_test.go/
// terminal_unix_test.go — exercises applyShellIntegration/integrationDir
// directly rather than through Manager, since there's no other way to
// observe the argv/env shape spawn() builds without actually spawning a
// real shell for each case.

import (
	"os/exec"
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
	argv, env := applyShellIntegration([]string{"/bin/bash"}, []string{"FOO=bar"})
	if argv[0] != "/bin/bash" {
		t.Fatalf("expected argv[0] untouched, got %q", argv[0])
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--rcfile") || !strings.Contains(joined, "integration.bash") {
		t.Fatalf("expected --rcfile integration.bash in argv, got %v", argv)
	}
	if len(env) != 1 || env[0] != "FOO=bar" {
		t.Fatalf("expected bash's env untouched aside from what was already there, got %v", env)
	}
}

// TestApplyShellIntegration_Zsh guards the ZDOTDIR redirect: our dir goes
// in as ZDOTDIR, and the real one (however it started, including empty for
// "unset") is preserved separately so integration.zshrc can chain back to
// the user's actual config.
func TestApplyShellIntegration_Zsh(t *testing.T) {
	argv, env := applyShellIntegration([]string{"/usr/bin/zsh"}, nil)
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
	argv, env := applyShellIntegration([]string{"/bin/sh"}, []string{"FOO=bar"})
	if len(argv) != 1 || argv[0] != "/bin/sh" {
		t.Fatalf("expected /bin/sh's argv untouched, got %v", argv)
	}
	if len(env) != 1 || env[0] != "FOO=bar" {
		t.Fatalf("expected /bin/sh's env untouched, got %v", env)
	}
}
