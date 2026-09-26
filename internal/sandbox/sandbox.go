package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

type SandboxMode string

const (
	ModeBubblewrap SandboxMode = "bubblewrap"
	ModeNamespace  SandboxMode = "linux_namespaces"
	// ModeSeatbelt is macOS's sandbox-exec (Seatbelt) tier — a real,
	// kernel-enforced deny-by-default filesystem policy, the closest macOS
	// equivalent to bubblewrap. Given its own honest label rather than
	// reusing ModeBubblewrap (misleading — it isn't bubblewrap) or
	// ModeLocalRestr (would underclaim real protection it does provide).
	ModeSeatbelt   SandboxMode = "macos_seatbelt"
	ModeLocalRestr SandboxMode = "local_restricted_fallback"
)

type ExecutionResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	ModeUsed SandboxMode
}

// PTYSession is a long-lived, interactive counterpart to Execute: a process
// attached to a pseudo-terminal instead of buffered stdout/stderr, for the
// terminal feature (an actual shell a human drives, as opposed to a single
// agent-issued command). Read/Write go through the pty master.
type PTYSession interface {
	io.ReadWriter
	// Resize applies a new terminal window size — a client resizing its
	// view (e.g. a split pane) calls this, not a fresh spawn.
	Resize(cols, rows int) error
	// Close terminates the session (closing the pty master delivers SIGHUP
	// to the foreground process group, same as closing a real terminal
	// window; a direct kill follows as a backstop). Does not reap the
	// process — call Wait for that, exactly once, from whichever goroutine
	// owns reading the session's output.
	Close() error
	// Wait blocks until the child exits and reaps it, returning its exit
	// code (-1 if it was killed by a signal rather than exiting normally).
	Wait() (int, error)
	Pid() int
}

type Sandbox interface {
	Mode() SandboxMode
	// Execute runs argv inside the sandbox. extraEnv, if non-nil, is
	// appended to the base environment as-is (e.g. "GIT_AUTHOR_NAME=...")
	// — nil means no additions, today's exact behavior.
	Execute(ctx context.Context, argv []string, extraEnv []string) (*ExecutionResult, error)
	// SpawnPTY starts argv as an interactive process attached to a pty,
	// under the same isolation tier Execute uses, sized to cols x rows.
	// Unlike Execute, ctx is not given a deadline internally — the session
	// runs until the process exits or the caller cancels ctx/Close()s it.
	// dir is where it starts, already validated to be inside the project
	// root; "" means the root itself.
	SpawnPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int) (PTYSession, error)
}

// Helper to determine if an executable is in $PATH
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// ShellIntegrationDir is where the terminal package writes its shell rc scripts. It lives under
// the host's temp dir, which bubblewrap hides behind a fresh tmpfs, so SpawnPTY binds this one
// directory back in read-only.
func ShellIntegrationDir() string {
	return filepath.Join(os.TempDir(), "yoakeland-shell-integration")
}
