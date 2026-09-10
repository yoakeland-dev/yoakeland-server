//go:build !linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
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

func (s *NonLinuxSandbox) Execute(ctx context.Context, argv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.Dir = s.projectRoot
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
