//go:build linux || darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/creack/pty"
)

// ErrHostUnsupported: see host_other.go. Never returned here.
var ErrHostUnsupported = errors.New("unconfined terminals aren't supported on this platform")

// ModeHost labels a terminal that runs as the person's own shell, outside any sandbox.
const ModeHost SandboxMode = "host"

// SpawnHostPTY starts argv on a pty as the daemon's own user with no confinement: the real
// environment, home directory and dotfiles. For a terminal a person opened themselves (only
// owner-level peers can; see remote_scope.go), where a sandbox just gets in the way. The agent's
// terminals never come through here. extraEnv is appended last, so it wins.
func SpawnHostPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int) (PTYSession, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	env := []string{"TERM=xterm-256color"}
	for _, kv := range os.Environ() {
		// The daemon's own settings and secrets are not the shell's business.
		if !strings.HasPrefix(kv, "YOAKE") {
			env = append(env, kv)
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(env, extraEnv...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("pty spawn error: %w", err)
	}
	return &hostPTYSession{cmd: cmd, ptmx: ptmx}, nil
}

type hostPTYSession struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	waited atomic.Bool
}

func (p *hostPTYSession) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *hostPTYSession) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p *hostPTYSession) Pid() int                    { return p.cmd.Process.Pid }
func (p *hostPTYSession) Foreground() (int, bool)     { return ttyForeground(p.ptmx) }

func (p *hostPTYSession) Resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Close hangs up the terminal like closing its window: the shell gets SIGHUP and passes it to its
// jobs; what the shell started is ended with it (a daemon that reparented itself to init is out of reach).
func (p *hostPTYSession) Close() error {
	// Before the hangup: once the shell is gone its jobs are orphans nobody can find by parentage.
	live := p.cmd.Process != nil && !p.waited.Load()
	if live {
		killTree(p.cmd.Process.Pid)
	}
	err := p.ptmx.Close()
	if live && !p.waited.Load() {
		_ = p.cmd.Process.Kill()
	}
	return err
}

func (p *hostPTYSession) Wait() (int, error) {
	err := p.cmd.Wait()
	p.waited.Store(true)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
