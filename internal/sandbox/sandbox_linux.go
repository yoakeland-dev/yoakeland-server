//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type LinuxSandbox struct {
	projectRoot string
	timeout     time.Duration
	mode        SandboxMode
}

func NewSandbox(projectRoot string, defaultTimeout time.Duration) Sandbox {
	mode := ModeLocalRestr
	if probeBubblewrap(projectRoot) {
		mode = ModeBubblewrap
	} else if probeNamespaces(projectRoot) {
		mode = ModeNamespace
	}

	return &LinuxSandbox{
		projectRoot: projectRoot,
		timeout:     defaultTimeout,
		mode:        mode,
	}
}

// Mode reports the sandbox tier as it should be understood by a caller
// deciding how much to trust it (project_opened.sandboxTier, and the
// startup warning log) — as opposed to s.mode, which drives Execute()'s
// internal branching and can be more optimistic than what's actually safe
// to report.
//
// The namespace-only fallback (CLONE_NEWPID|NEWNS|NEWUTS|NEWIPC, no
// CLONE_NEWNET, no mount/root restriction) isolates the process tree but
// grants no real network or filesystem isolation — a command running under
// it can still reach the network and read/write anything this OS user can.
// Reporting it as "linux_namespaces" would overclaim a level of isolation
// it doesn't provide, so it's demoted to the same "local_restricted_fallback"
// label the no-isolation-at-all case gets, per SPEC.md's own
// insecure-by-omission-is-always-visible principle. Execute() still applies
// those namespace flags regardless — free defense in depth — this only
// changes what's reported, not what's run.
func (s *LinuxSandbox) Mode() SandboxMode {
	if s.mode == ModeNamespace {
		return ModeLocalRestr
	}
	return s.mode
}

func (s *LinuxSandbox) Execute(ctx context.Context, argv []string, extraEnv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// Resolve bare executable names (e.g. "echo" -> "/usr/bin/echo")
	execArgv := make([]string, len(argv))
	copy(execArgv, argv)
	if !filepath.IsAbs(execArgv[0]) {
		if lp, err := exec.LookPath(execArgv[0]); err == nil {
			execArgv[0] = lp
		}
	}

	var cmd *exec.Cmd

	if s.mode == ModeBubblewrap {
		bwrapArgs := []string{
			"--ro-bind", "/usr", "/usr",
		}

		// Recreate host usrmerge symlinks (/bin -> usr/bin, /lib -> usr/lib, etc.)
		for _, sysPath := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
			fi, err := os.Lstat(sysPath)
			if err != nil {
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				linkTarget, err := os.Readlink(sysPath)
				if err == nil {
					cleanTarget := strings.TrimPrefix(linkTarget, "/")
					bwrapArgs = append(bwrapArgs, "--symlink", cleanTarget, sysPath)
				}
			} else if fi.IsDir() {
				bwrapArgs = append(bwrapArgs, "--ro-bind", sysPath, sysPath)
			}
		}

		if _, err := os.Stat("/etc"); err == nil {
			bwrapArgs = append(bwrapArgs, "--ro-bind", "/etc", "/etc")
		}

		bwrapArgs = append(bwrapArgs,
			"--proc", "/proc",
			"--dev", "/dev",
			"--tmpfs", "/tmp",
			"--bind", s.projectRoot, s.projectRoot,
			"--chdir", s.projectRoot,
			"--unshare-pid",
			"--unshare-ipc",
			"--unshare-uts",
			"--unshare-net",
			"--die-with-parent",
			// Puts the sandboxed process in its own terminal session so it
			// has no controlling tty inherited from yoakeland's own. Without
			// this, a sandboxed command can use the TIOCSTI ioctl to push
			// synthetic keystrokes onto the parent's terminal — a
			// well-documented bwrap/container escape technique (the same
			// class of issue Flatpak's own bwrap invocation guards against)
			// that has nothing to do with filesystem/network isolation and
			// isn't covered by any of the --unshare-* flags above.
			"--new-session",
			"--",
		)
		bwrapArgs = append(bwrapArgs, execArgv...)
		cmd = exec.CommandContext(execCtx, "bwrap", bwrapArgs...)
	} else if s.mode == ModeNamespace {
		cmd = exec.CommandContext(execCtx, execArgv[0], execArgv[1:]...)
		cmd.Dir = s.projectRoot
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		}
	} else {
		cmd = exec.CommandContext(execCtx, execArgv[0], execArgv[1:]...)
		cmd.Dir = s.projectRoot
	}

	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"HOME=" + s.projectRoot,
		"PWD=" + s.projectRoot,
	}, extraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Prioritize context deadline / cancellation errors
	if execCtx.Err() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("execution timed out after %v: %w", s.timeout, execCtx.Err())
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("sandbox launch error: %w", err)
		}
	}

	return &ExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		ModeUsed: s.mode,
	}, nil
}

func probeBubblewrap(testDir string) bool {
	if !commandExists("bwrap") {
		return false
	}

	checkBubblewrapVersion()

	args := []string{"--ro-bind", "/usr", "/usr"}
	for _, sysPath := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		fi, err := os.Lstat(sysPath)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(sysPath)
			if err == nil {
				args = append(args, "--symlink", strings.TrimPrefix(linkTarget, "/"), sysPath)
			}
		} else if fi.IsDir() {
			args = append(args, "--ro-bind", sysPath, sysPath)
		}
	}

	if _, err := os.Stat("/etc"); err == nil {
		args = append(args, "--ro-bind", "/etc", "/etc")
	}

	targetBin := "/bin/true"
	if lp, err := exec.LookPath("true"); err == nil {
		targetBin = lp
	}

	args = append(args,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", testDir, testDir,
		"--chdir", testDir,
		"--die-with-parent",
		"--new-session",
		"--",
		targetBin,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	return cmd.Run() == nil
}

// checkBubblewrapVersion warns (but never fails) if the installed bwrap is
// older than 0.12.0, or if its version can't be determined at all. Older
// bubblewrap releases are known-vulnerable: GHSA-pxhw-h44j-8pfx is a
// symlink-traversal sandbox escape fixed in 0.12.0, and GHSA-xq78-7hw4-5jvp
// is a ptrace privilege-escalation issue in setuid installs fixed in
// 0.11.2. This is warn-only, consistent with how other insecure-by-default
// states (no TLS, no master key) are surfaced elsewhere in this codebase.
func checkBubblewrapVersion() {
	out, err := exec.Command("bwrap", "--version").Output()
	if err != nil {
		log.Printf("WARNING: could not determine bubblewrap version (%v) — if it predates 0.12.0 it may be vulnerable to known sandbox-escape CVEs (GHSA-pxhw-h44j-8pfx, GHSA-xq78-7hw4-5jvp). Consider upgrading bubblewrap.", err)
		return
	}

	fields := strings.Fields(string(out))
	version := ""
	for _, f := range fields {
		if len(f) > 0 && (f[0] >= '0' && f[0] <= '9') {
			version = f
			break
		}
	}
	if version == "" || !bubblewrapVersionAtLeast(version, [3]int{0, 12, 0}) {
		log.Printf("WARNING: bubblewrap version %q may be vulnerable to known sandbox-escape CVEs — GHSA-pxhw-h44j-8pfx (symlink-traversal escape, fixed in 0.12.0) and GHSA-xq78-7hw4-5jvp (ptrace privilege escalation in setuid installs, fixed in 0.11.2). Recommend upgrading bubblewrap.", strings.TrimSpace(string(out)))
	}
}

// bubblewrapVersionAtLeast reports whether version (e.g. "0.11.3") is >=
// min. An unparseable version is treated as not meeting the minimum, so the
// caller warns rather than silently trusting it.
func bubblewrapVersionAtLeast(version string, min [3]int) bool {
	parts := strings.SplitN(version, ".", 3)
	var got [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		got[i] = n
	}
	for i := 0; i < 3; i++ {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true
}

func probeNamespaces(testDir string) bool {
	targetBin := "/bin/true"
	if lp, err := exec.LookPath("true"); err == nil {
		targetBin = lp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, targetBin)
	cmd.Dir = testDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
	}
	return cmd.Run() == nil
}
