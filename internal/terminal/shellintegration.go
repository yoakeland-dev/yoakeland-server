package terminal

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"yoakeland/internal/sandbox"
)

// Shell integration injects a small init script into the shell we spawn so
// it emits a subset of VS Code's OSC 633 protocol (prompt/command
// boundaries, exit codes, cwd) on the same raw terminal_output byte stream
// every other pty write already rides — no wire protocol change, the
// frontend's terminalShellIntegration.ts just parses bytes that were always
// flowing through. See the embedded scripts themselves for the OSC 633
// subset chosen and why (a scope cut from VS Code's full set).
//
// Bash and zsh only — anything else (sh, fish, pwsh, cmd) is left exactly
// as before; applyShellIntegration's shell-name switch falls through
// silently for them.

//go:embed scripts/integration.bash
var bashIntegrationScript []byte

//go:embed scripts/integration.zshenv
var zshIntegrationZshenv []byte

//go:embed scripts/integration.zshrc
var zshIntegrationZshrc []byte

var writeIntegrationScriptsOnce sync.Once
var integrationScriptDir string
var integrationScriptErr error

// integrationDir returns the directory the embedded scripts have been
// written to on disk (bash/zsh can only source a real file, not embedded
// bytes), writing them exactly once per process. Written under os.TempDir()
// and never cleaned up by us — same reasoning as not persisting a
// terminal's history beyond the daemon's own lifetime: it's a handful of
// small, static, non-secret files the OS already reclaims.
func integrationDir() (string, error) {
	writeIntegrationScriptsOnce.Do(func() {
		dir := sandbox.ShellIntegrationDir()
		if dir == "" {
			integrationScriptErr = errors.New("no private temp dir for shell integration")
			return
		}
		files := map[string][]byte{
			"integration.bash":   bashIntegrationScript,
			"integration.zshenv": zshIntegrationZshenv,
			"integration.zshrc":  zshIntegrationZshrc,
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
				integrationScriptErr = err
				return
			}
		}
		integrationScriptDir = dir
	})
	return integrationScriptDir, integrationScriptErr
}

// newMarkNonce is the secret each terminal's marks are stamped with (see
// scanMarks). It travels to the shell in YOAKE_MARK_NONCE, which the scripts
// unset at once so the commands they run don't inherit it.
func newMarkNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// applyShellIntegration extends argv/env to inject OSC 633 shell
// integration for the shells we know how to do it for, keyed off the
// shell's own basename (argv[0], as shellArgv chose it) — bash gets
// --rcfile pointed at our script (see integration.bash for why that's safe
// for the user's own ~/.bashrc), zsh gets ZDOTDIR redirected at our script
// dir (see integration.zshenv/.zshrc for the chain-through). Any other
// shell — including on Windows, where the basename never matches either —
// is returned unmodified: this is intentionally not attempted for
// fish/pwsh/cmd (see the package doc comment on scope).
//
// A failure to write the scripts to disk (integrationDir) degrades to "no
// shell integration" rather than failing the spawn — a terminal a human is
// about to type into is far more valuable than the OSC 633 decorations
// riding on top of it. nonce stamps the marks (see newMarkNonce).
func applyShellIntegration(argv []string, env []string, nonce string) ([]string, []string) {
	if len(argv) == 0 {
		return argv, env
	}
	dir, err := integrationDir()
	if err != nil {
		return argv, env
	}

	switch filepath.Base(argv[0]) {
	case "bash":
		argv = append(append([]string{}, argv...), "--rcfile", filepath.Join(dir, "integration.bash"))
		env = append(append([]string{}, env...), "YOAKE_MARK_NONCE="+nonce)
	case "zsh":
		origZdotdir := os.Getenv("ZDOTDIR")
		env = append(append([]string{}, env...), "ZDOTDIR="+dir, "YOAKE_ORIG_ZDOTDIR="+origZdotdir, "YOAKE_MARK_NONCE="+nonce)
	}
	return argv, env
}
