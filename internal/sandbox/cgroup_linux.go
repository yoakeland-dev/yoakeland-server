//go:build linux

package sandbox

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Resource caps applied to a spawned command or terminal session when cgroup
// v2 delegation is available (see probeCgroupDelegation) come from Limits
// (runtime settings terminalMemoryMaxMB / terminalPidsMax) at spawn time.

var (
	cgroupProbeOnce sync.Once
	cgroupOwnPath   string // this process's own cgroup v2 path, e.g. "/user.slice/...", empty if unusable
	cgroupUsable    bool

	cgroupWarnOnce sync.Once
)

// probeCgroupDelegation checks, once per process lifetime, whether this
// process can create and configure its own cgroup v2 children — the
// "delegation" a systemd user session (or a rootless container runtime)
// typically grants, and which a bare daemon with no session manager above it
// (this project's common case — see the Artix/no-systemd dev environment
// this was built against) typically does not have. The result is cached:
// every terminal spawn calling this doesn't re-touch the filesystem.
func probeCgroupDelegation() (string, bool) {
	cgroupProbeOnce.Do(func() {
		raw, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return
		}
		// Unified (v2) mode is exactly one line, "0::<path>". Anything else
		// (multiple lines, a nonzero hierarchy id) is a v1/hybrid mount,
		// which this doesn't support — not worth the extra complexity for a
		// best-effort resource cap.
		line := strings.TrimSpace(string(raw))
		if strings.Count(line, "\n") != 0 {
			return
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			return
		}

		testDir := filepath.Join("/sys/fs/cgroup", parts[2], "yoakeland-probe")
		if err := os.Mkdir(testDir, 0o755); err != nil {
			return
		}
		_ = os.Remove(testDir)

		cgroupOwnPath = parts[2]
		cgroupUsable = true
	})
	return cgroupOwnPath, cgroupUsable
}

// cgroupLeaf is a fresh cgroup v2 leaf with memory and pids caps, made before
// its command starts. The command is born inside it (clone3's
// CLONE_INTO_CGROUP, via SysProcAttr.CgroupFD): moving an already-running pid
// in left a window in which its first children ran uncapped.
type cgroupLeaf struct {
	dir string
	fd  *os.File // the leaf's directory, open until the command has started
}

var cgroupLeafSeq atomic.Uint64

// newCgroupLeaf returns nil when there is no delegation or the leaf can't be
// configured, so the command runs without the caps rather than not at all.
// Every step's error is checked and a partial leaf is torn down, so nothing is
// left looking capped when it isn't. Logs a warning at most once per process
// when delegation itself isn't available (the expected case on a bare daemon).
func newCgroupLeaf() *cgroupLeaf {
	ownPath, ok := probeCgroupDelegation()
	if !ok {
		cgroupWarnOnce.Do(func() {
			log.Printf("WARNING: no cgroup v2 delegation. Terminals and one-shot commands have no memory or process-count limit beyond what bubblewrap and namespaces isolate, so a runaway or malicious command can exhaust host memory or fork-bomb it. Run this daemon under a systemd user session, or grant it cgroup delegation, to enforce limits.")
		})
		return nil
	}

	dir := filepath.Join("/sys/fs/cgroup", ownPath, fmt.Sprintf("yoakeland-%d-%d", os.Getpid(), cgroupLeafSeq.Add(1)))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil
	}
	l := &cgroupLeaf{dir: dir}

	// A cgroup only exposes memory.max/pids.max to a child once the parent
	// delegates those controllers via its own subtree_control — best-effort,
	// since a parent that already has member processes of its own can hit
	// cgroup v2's "no internal process" constraint here and refuse it, which
	// the write-and-verify below catches rather than assumes away.
	_ = os.WriteFile(filepath.Join("/sys/fs/cgroup", ownPath, "cgroup.subtree_control"), []byte("+memory +pids"), 0o644)

	caps := CurrentLimits()
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(caps.TerminalMemoryBytes, 10)), 0o644); err != nil {
		l.remove()
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.Itoa(caps.TerminalPids)), 0o644); err != nil {
		l.remove()
		return nil
	}
	fd, err := os.Open(dir)
	if err != nil {
		l.remove()
		return nil
	}
	l.fd = fd
	return l
}

// place makes cmd start inside the leaf. A nil leaf leaves cmd as it is.
func (l *cgroupLeaf) place(cmd *exec.Cmd) {
	if l == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(l.fd.Fd())
}

// release closes the leaf's directory handle, which only placing needs.
func (l *cgroupLeaf) release() {
	if l != nil && l.fd != nil {
		_ = l.fd.Close()
		l.fd = nil
	}
}

// remove deletes the leaf once its command has exited. It fails harmlessly
// while processes are still in it.
func (l *cgroupLeaf) remove() {
	if l == nil {
		return
	}
	l.release()
	_ = os.Remove(l.dir)
}
