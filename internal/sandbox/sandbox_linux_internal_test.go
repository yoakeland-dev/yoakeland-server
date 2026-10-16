//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLinuxSandbox_Mode_DemotesNamespaceOnlyToLocalRestricted guards against
// overclaiming isolation: the namespace-only fallback (no CLONE_NEWNET, no
// mount/root restriction) must be reported as "local_restricted_fallback",
// not "linux_namespaces", since it isolates the process tree but not the
// network or filesystem — a client relying on SPEC.md's guidance to warn
// only on local_restricted_fallback would otherwise show no warning for a
// project that is, in practice, running bash with full host network and
// filesystem access.
func TestLinuxSandbox_Mode_DemotesNamespaceOnlyToLocalRestricted(t *testing.T) {
	sb := &LinuxSandbox{mode: ModeNamespace}
	if got := sb.Mode(); got != ModeLocalRestr {
		t.Fatalf("Mode() = %q, want %q (namespace-only fallback must not be reported as genuinely isolated)", got, ModeLocalRestr)
	}
}

// TestLinuxSandbox_Mode_ReportsBubblewrapAndFallbackAsIs guards that the
// demotion above is narrowly scoped to ModeNamespace — bubblewrap (which
// does isolate network/filesystem via --unshare-net and an explicit bind
// allowlist) and the already-honest no-isolation fallback must be reported
// unchanged.
func TestLinuxSandbox_Mode_ReportsBubblewrapAndFallbackAsIs(t *testing.T) {
	if got := (&LinuxSandbox{mode: ModeBubblewrap}).Mode(); got != ModeBubblewrap {
		t.Errorf("Mode() = %q, want %q", got, ModeBubblewrap)
	}
	if got := (&LinuxSandbox{mode: ModeLocalRestr}).Mode(); got != ModeLocalRestr {
		t.Errorf("Mode() = %q, want %q", got, ModeLocalRestr)
	}
}

// TestCgroupLeaf_PlaceStartsCommandInside guards the cap ordering: a command
// is handed its leaf's fd before it starts, so it is born inside the cgroup
// rather than moved there after its first children may have forked. A nil
// leaf (no delegation) must leave the command alone.
func TestCgroupLeaf_PlaceStartsCommandInside(t *testing.T) {
	cmd := &exec.Cmd{Path: "/bin/true"}
	var nilLeaf *cgroupLeaf
	nilLeaf.place(cmd)
	nilLeaf.release()
	nilLeaf.remove()
	if cmd.SysProcAttr != nil {
		t.Fatal("a nil leaf must not touch the command")
	}

	fd, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	leaf := &cgroupLeaf{fd: fd}
	leaf.place(cmd)
	if !cmd.SysProcAttr.UseCgroupFD || cmd.SysProcAttr.CgroupFD != int(fd.Fd()) {
		t.Fatalf("SysProcAttr = %+v, want UseCgroupFD with the leaf's fd", cmd.SysProcAttr)
	}
}

// TestProbeBubblewrap_RetriesASlowFirstRun: a first run past the one-second
// deadline (a cold cache, a loaded host) must not read as "no bubblewrap" for
// the daemon's whole lifetime. A fake bwrap is slow once, then works.
func TestProbeBubblewrap_RetriesASlowFirstRun(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran-once")
	// PATH is replaced below, so the script names sleep by absolute path and
	// creates the marker with a builtin.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'bubblewrap 0.12.0'; exit 0; fi\n" +
		"if [ ! -e " + marker + " ]; then : > " + marker + "; " + sleep + " 3; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "bwrap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	start := time.Now()
	if !probeBubblewrap(t.TempDir()) {
		t.Fatal("probeBubblewrap gave up after one slow run; it should retry")
	}
	if _, err := os.Stat(marker); err != nil || time.Since(start) < time.Second {
		t.Fatalf("the slow first run never happened (marker %v, took %v): the test proves nothing", err, time.Since(start))
	}
}
