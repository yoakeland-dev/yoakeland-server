package sandbox

import (
	"context"
	"os/exec"
)

type SandboxMode string

const (
	ModeBubblewrap SandboxMode = "bubblewrap"
	ModeNamespace  SandboxMode = "linux_namespaces"
	ModeLocalRestr SandboxMode = "local_restricted_fallback"
)

type ExecutionResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	ModeUsed SandboxMode
}

type Sandbox interface {
	Mode() SandboxMode
	Execute(ctx context.Context, argv []string) (*ExecutionResult, error)
}

// Helper to determine if an executable is in $PATH
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}
