//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (s *LinuxSandbox) Mode() SandboxMode {
	return s.mode
}

func (s *LinuxSandbox) Execute(ctx context.Context, argv []string) (*ExecutionResult, error) {
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

	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"HOME=" + s.projectRoot,
		"PWD=" + s.projectRoot,
	}

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
		"--",
		targetBin,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	return cmd.Run() == nil
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
