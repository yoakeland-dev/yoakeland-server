//go:build linux

package sandbox

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Resource caps applied to a spawned terminal session when cgroup v2
// delegation is available (see probeCgroupDelegation) come from Limits
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

// applyCgroupLimits best-effort confines pid to a fresh cgroup v2 leaf with
// memory/pids caps — the upgrade path buildCmd's own doc comment points to
// for the "no seccomp filter, no cgroup limits" gap. Every step's error is
// checked and any failure tears down what was created and reports
// applied=false rather than silently leaving pid in an unconfined cgroup
// while claiming success: an unenforced cap that looks enforced is worse
// than an honest "no limit" (same insecure-by-omission-is-visible rule the
// bubblewrap-version and sandbox-tier warnings already follow elsewhere in
// this package). Logs a warning at most once per process when delegation
// itself isn't available — the expected case on a bare daemon — rather than
// once per terminal.
func applyCgroupLimits(pid int) (cleanup func(), applied bool) {
	noop := func() {}

	ownPath, ok := probeCgroupDelegation()
	if !ok {
		cgroupWarnOnce.Do(func() {
			log.Printf("WARNING: no cgroup v2 delegation available — terminal sessions run with no memory/process-count limit beyond what bubblewrap/namespaces already isolate. A runaway or malicious interactive shell can still exhaust host memory or fork-bomb it. Run this daemon under a systemd user session (or grant it cgroup delegation another way) for enforced resource limits.")
		})
		return noop, false
	}

	dir := filepath.Join("/sys/fs/cgroup", ownPath, fmt.Sprintf("yoakeland-term-%d", pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return noop, false
	}
	cleanupDir := func() { _ = os.Remove(dir) }

	// A cgroup only exposes memory.max/pids.max to a child once the parent
	// delegates those controllers via its own subtree_control — best-effort,
	// since a parent that already has member processes of its own can hit
	// cgroup v2's "no internal process" constraint here and refuse it, which
	// the write-and-verify below catches rather than assumes away.
	parentSubtree := filepath.Join("/sys/fs/cgroup", ownPath, "cgroup.subtree_control")
	_ = os.WriteFile(parentSubtree, []byte("+memory +pids"), 0o644)

	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(CurrentLimits().TerminalMemoryBytes, 10)), 0o644); err != nil {
		cleanupDir()
		return noop, false
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.Itoa(CurrentLimits().TerminalPids)), 0o644); err != nil {
		cleanupDir()
		return noop, false
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		cleanupDir()
		return noop, false
	}

	return cleanupDir, true
}
