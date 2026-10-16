package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A CLI installed after the daemon started, into a folder only the login
// shell's PATH has, is found within cliPathTTL and run with that PATH.
func TestLookCLIFindsACLIInstalledSinceStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no login shell PATH on Windows")
	}
	shellDir, daemonDir := t.TempDir(), t.TempDir()
	t.Setenv("PATH", daemonDir)
	t.Setenv("HOME", t.TempDir())
	old := shellPath
	shellPath = func() string { return shellDir }
	t.Cleanup(func() { shellPath = old; cliPathVal = "" })
	cliPathVal = ""

	if _, err := lookCLI("fakeagent"); err == nil {
		t.Fatal("found a CLI that isn't installed")
	}
	bin := filepath.Join(shellDir, "fakeagent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := lookCLI("fakeagent"); err != nil || got != bin {
		t.Fatalf("lookCLI = %q, %v; want %q", got, err, bin)
	}

	// The shell's PATH moves on (a new install dir): picked up once the TTL passes.
	newDir := t.TempDir()
	shellPath = func() string { return newDir }
	cliPathTime = time.Now().Add(-cliPathTTL)
	env := withCLIPath([]string{"PATH=/old", "HOME=/h"})
	if last := env[len(env)-1]; !strings.HasPrefix(last, "PATH="+newDir) || strings.Contains(strings.Join(env, " "), "/old") {
		t.Fatalf("env = %v, want PATH led by %s and the old PATH gone", env, newDir)
	}
}

func TestShellPathReadsTheLoginShell(t *testing.T) {
	sh, err := os.Stat("/bin/sh")
	if runtime.GOOS == "windows" || err != nil || sh.IsDir() {
		t.Skip("no /bin/sh")
	}
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	if got := shellPath(); !strings.Contains(got, "/bin") {
		t.Fatalf("shellPath = %q", got)
	}
}
