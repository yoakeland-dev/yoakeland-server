package sandbox

// =============================================================================
// SANDBOX SECURITY ADVISORY (Phase 0):
// The boundary below locks working directory (cwd) and sanitizes environment
// variables to prevent accidental leaks.
//
// WARNING: This is INSUFFICIENT to isolate untrusted agentic actions.
// Subprocesses inherit local user privileges and system access. Real sandboxing
// (e.g. Linux namespaces, bubblewrap, or OCI containers) is a strict requirement
// before executing untrusted inputs or scripts.
// =============================================================================

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type ExecutionResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type LocalSandbox struct {
	ProjectRoot string
	Timeout     time.Duration
}

func NewLocalSandbox(projectRoot string, defaultTimeout time.Duration) *LocalSandbox {
	return &LocalSandbox{
		ProjectRoot: projectRoot,
		Timeout:     defaultTimeout,
	}
}

func (s *LocalSandbox) Execute(ctx context.Context, argv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command argv")
	}

	execCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.Dir = s.ProjectRoot

	// Sanitized minimum viable environment
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		fmt.Sprintf("HOME=%s", s.ProjectRoot),
		fmt.Sprintf("PWD=%s", s.ProjectRoot),
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
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
	}, nil
}
