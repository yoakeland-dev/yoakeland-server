package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The daemon's own PATH is whatever it was started with: from a desktop
// launcher that is often a bare system PATH, and it never changes after. A CLI
// installed since (or into ~/.local/bin, ~/.bun/bin, an nvm node...) would stay
// invisible until a restart, so CLIs are looked up on a PATH rebuilt from the
// user's login shell plus the usual install folders, refreshed every
// cliPathTTL.
const cliPathTTL = 30 * time.Second

var (
	cliPathMu   sync.Mutex
	cliPathVal  string
	cliPathTime time.Time
	cliPathBase string // the daemon PATH cliPathVal was built from
)

// shellPath is the user's login shell's PATH, or "" if it can't be had in time.
// A var so tests can stub it.
var shellPath = func() string {
	sh := os.Getenv("SHELL")
	if sh == "" || runtime.GOOS == "windows" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Interactive too: nvm, mise and friends usually hook in from .bashrc/.zshrc.
	// The marker picks PATH out of whatever else the rc files print.
	cmd := exec.CommandContext(ctx, sh, "-lic", `printf '\n__YOAKE_PATH__=%s\n' "$PATH"`)
	cmd.Stdin = nil
	cmd.Dir = os.Getenv("HOME")
	ownProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "__YOAKE_PATH__="); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// wellKnownBinDirs are where CLI installers put things even when the shell
// that would add them to PATH isn't asked.
func wellKnownBinDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			dirs = append(dirs, filepath.Join(d, "npm"))
		}
		return dirs
	}
	if home != "" {
		for _, d := range []string{".local/bin", "bin", ".bun/bin", ".npm-global/bin", ".cargo/bin", "go/bin", ".deno/bin", ".volta/bin", ".claude/local", ".opencode/bin", ".amp/bin", ".grok/bin"} {
			dirs = append(dirs, filepath.Join(home, d))
		}
	}
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
}

// cliPath is the PATH CLIs are found and run with: the login shell's, then the
// daemon's own, then the well-known folders, without repeats.
// YOAKELAND_STRICT_PATH set keeps it to the daemon's own PATH.
func cliPath() string {
	base := os.Getenv("PATH")
	if os.Getenv("YOAKELAND_STRICT_PATH") != "" {
		return base
	}
	cliPathMu.Lock()
	defer cliPathMu.Unlock()
	if cliPathVal != "" && base == cliPathBase && time.Since(cliPathTime) < cliPathTTL {
		return cliPathVal
	}
	seen := map[string]bool{}
	var dirs []string
	add := func(list ...string) {
		for _, d := range list {
			if d != "" && !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	add(filepath.SplitList(shellPath())...)
	add(filepath.SplitList(base)...)
	// Listed even before they exist: each lookup checks the folders afresh, so an
	// installer that creates ~/.local/bin is seen at once, not on the next rebuild.
	add(wellKnownBinDirs()...)
	cliPathVal, cliPathTime, cliPathBase = strings.Join(dirs, string(os.PathListSeparator)), time.Now(), base
	return cliPathVal
}

// lookCLI finds name the way exec.LookPath would, but on cliPath.
func lookCLI(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(cliPath()) {
		if p, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// withCLIPath replaces PATH in env with cliPath, so a CLI's own children (the
// node behind an npm-installed agent, say) are found the same way it was.
func withCLIPath(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "PATH") { // Windows spells it Path
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+cliPath())
}
