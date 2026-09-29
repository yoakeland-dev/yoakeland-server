//go:build !linux && !darwin && !windows

// This is the true last-resort tier: any OS other than Linux (bubblewrap/
// namespaces, sandbox_linux.go), macOS (sandbox-exec, sandbox_darwin.go), or
// Windows (Job Objects, sandbox_windows.go) — e.g. the BSDs, Solaris. No
// isolation at all, same as ModeLocalRestr means everywhere else in this
// package. creack/pty has real support for most of these (see its
// pty_freebsd.go/pty_openbsd.go/etc.), so SpawnPTY still works, just
// unconfined.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
)

type NonLinuxSandbox struct {
	projectRoot string
	timeout     time.Duration
}

func NewSandbox(projectRoot string, defaultTimeout time.Duration) Sandbox {
	return &NonLinuxSandbox{
		projectRoot: projectRoot,
		timeout:     defaultTimeout,
	}
}

func (s *NonLinuxSandbox) Mode() SandboxMode {
	return ModeLocalRestr
}

func (s *NonLinuxSandbox) Execute(ctx context.Context, argv []string, extraEnv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.Dir = s.projectRoot
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
			return nil, fmt.Errorf("process execution failed: %w", err)
		}
	}

	return &ExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		ModeUsed: ModeLocalRestr,
	}, nil
}

// SpawnPTY has no isolation to apply on this platform (see Execute) — same
// unconfined-fallback tier, just attached to a pty instead of buffered pipes.
func (s *NonLinuxSandbox) SpawnPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int, allowNetwork bool) (PTYSession, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.projectRoot
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"HOME=" + s.projectRoot,
		"PWD=" + s.projectRoot,
		"TERM=xterm-256color",
	}, extraEnv...)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("pty spawn error: %w", err)
	}

	return &otherPTYSession{cmd: cmd, ptmx: ptmx}, nil
}

type otherPTYSession struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func (p *otherPTYSession) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *otherPTYSession) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p *otherPTYSession) Pid() int                    { return p.cmd.Process.Pid }

func (p *otherPTYSession) Resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (p *otherPTYSession) Close() error {
	err := p.ptmx.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return err
}

func (p *otherPTYSession) Wait() (int, error) {
	err := p.cmd.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
