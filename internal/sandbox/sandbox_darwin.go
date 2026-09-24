//go:build darwin

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
)

// DarwinSandbox uses sandbox-exec (Seatbelt), the closest macOS equivalent
// to bubblewrap: a kernel-enforced, deny-by-default access-control profile
// applied per-process, rather than bubblewrap's namespace/bind-mount model.
// sandbox-exec is officially deprecated by Apple (no public replacement API
// has shipped for third-party use) but remains functional as of current
// macOS releases and is what several other sandboxing tools still rely on
// for lack of an alternative.
//
// This has not been run on real macOS hardware — this project's development
// happens on Linux — so treat it as a best-effort implementation of a
// documented API rather than a verified one. If shells fail to spawn at all
// under the sandboxed tier, the Seatbelt profile below (darwinSeatbeltProfile)
// is almost certainly the first place to look: it's permissive on reads
// across standard system paths (dyld/framework resolution needs this) and
// restrictive on writes (confined to the project root and standard temp
// dirs), which is the same shape bubblewrap's tier takes, but Seatbelt's
// profile language has sharp edges around mach-lookup/sysctl-read/iokit-open
// that are easy to get subtly wrong without a real device to iterate on.
type DarwinSandbox struct {
	projectRoot string
	timeout     time.Duration
	sandboxed   bool
}

func NewSandbox(projectRoot string, defaultTimeout time.Duration) Sandbox {
	sb := &DarwinSandbox{projectRoot: projectRoot, timeout: defaultTimeout}
	sb.sandboxed = probeSandboxExec(projectRoot)
	return sb
}

func (s *DarwinSandbox) Mode() SandboxMode {
	if s.sandboxed {
		return ModeSeatbelt
	}
	return ModeLocalRestr
}

func (s *DarwinSandbox) Execute(ctx context.Context, argv []string, extraEnv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// allowNetwork=false: same deliberate boundary on an agent-issued
	// command as the Linux tier's Execute — see buildCmd's doc comment.
	cmd := s.buildCmd(execCtx, argv, false)
	cmd.Env = s.baseEnv(extraEnv, false)

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
			return nil, fmt.Errorf("sandbox launch error: %w", err)
		}
	}

	return &ExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		ModeUsed: s.Mode(),
	}, nil
}

// SpawnPTY starts argv attached to a pty under this sandbox's tier.
// creack/pty has real Darwin support (unlike its Windows stub — see
// sandbox_windows.go), so this reuses the same pty.StartWithSize path Linux
// does. allowNetwork=true: this is the human's own terminal — see
// sandbox_linux.go's buildCmd doc comment for the full reasoning, identical
// here.
func (s *DarwinSandbox) SpawnPTY(ctx context.Context, argv []string, extraEnv []string, cols, rows int) (PTYSession, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := s.buildCmd(ctx, argv, true)
	cmd.Env = s.baseEnv(extraEnv, true)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("pty spawn error: %w", err)
	}

	return &darwinPTYSession{cmd: cmd, ptmx: ptmx}, nil
}

// buildCmd wraps argv in sandbox-exec when Seatbelt is usable (probed once
// at construction — see probeSandboxExec), falling back to a bare exec.Cmd
// otherwise. allowNetwork controls the profile's network policy — false
// (Execute) denies it, true (SpawnPTY) allows it (darwinSeatbeltProfile
// takes network as a parameter rather than hardcoding a deny, unlike
// bubblewrap's simple flag, since Seatbelt has no equivalent to a network
// namespace — it's a policy rule, not an unshare).
func (s *DarwinSandbox) buildCmd(ctx context.Context, argv []string, allowNetwork bool) *exec.Cmd {
	execArgv := make([]string, len(argv))
	copy(execArgv, argv)
	if !filepath.IsAbs(execArgv[0]) {
		if lp, err := exec.LookPath(execArgv[0]); err == nil {
			execArgv[0] = lp
		}
	}

	if !s.sandboxed {
		cmd := exec.CommandContext(ctx, execArgv[0], execArgv[1:]...)
		cmd.Dir = s.projectRoot
		return cmd
	}

	profile := darwinSeatbeltProfile(s.projectRoot, allowNetwork)
	args := append([]string{"-p", profile}, execArgv...)
	cmd := exec.CommandContext(ctx, "sandbox-exec", args...)
	cmd.Dir = s.projectRoot
	return cmd
}

func (s *DarwinSandbox) baseEnv(extraEnv []string, interactive bool) []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"HOME=" + s.projectRoot,
		"PWD=" + s.projectRoot,
		"TMPDIR=/private/tmp",
	}
	if interactive {
		env = append(env, "TERM=xterm-256color")
	}
	return append(env, extraEnv...)
}

type darwinPTYSession struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func (p *darwinPTYSession) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *darwinPTYSession) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p *darwinPTYSession) Pid() int                    { return p.cmd.Process.Pid }

func (p *darwinPTYSession) Resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (p *darwinPTYSession) Close() error {
	err := p.ptmx.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return err
}

func (p *darwinPTYSession) Wait() (int, error) {
	err := p.cmd.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// probeSandboxExec checks that the sandbox-exec binary exists and that a
// minimal profile can actually launch something under it — the same
// "probe, don't assume" pattern probeBubblewrap uses on Linux, since
// sandbox-exec's availability/behavior isn't guaranteed across macOS
// versions (it's been deprecated since macOS 10.14 with no removal date
// ever announced, but "deprecated" here has already meant "quietly stops
// working" for other long-deprecated Apple CLI tools before).
func probeSandboxExec(testDir string) bool {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return false
	}

	targetBin := "/bin/true"
	if lp, err := exec.LookPath("true"); err == nil {
		targetBin = lp
	}

	profile := darwinSeatbeltProfile(testDir, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", profile, targetBin)
	return cmd.Run() == nil
}

// darwinSeatbeltProfile builds a Seatbelt (sandbox-exec) policy: deny by
// default, read access to standard system paths (needed for dyld/framework
// resolution and any standard CLI tool to run at all), read-write confined
// to projectRoot and standard temp directories, network gated by
// allowNetwork. escapeSeatbeltString guards against projectRoot containing
// a `"` or `\` that would otherwise break out of the profile's string
// literal.
func darwinSeatbeltProfile(projectRoot string, allowNetwork bool) string {
	root := escapeSeatbeltString(projectRoot)
	network := "(deny network*)"
	if allowNetwork {
		network = "(allow network*)"
	}
	return `(version 1)
(deny default)
(allow process-fork)
(allow process-exec)
(allow signal (target self))
(allow file-read*
  (subpath "/usr")
  (subpath "/bin")
  (subpath "/sbin")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/private/etc")
  (subpath "/dev")
  (literal "/etc")
  (literal "/tmp")
  (literal "/var")
  (literal "/")
)
(allow file-read* file-write*
  (subpath "` + root + `")
  (subpath "/private/tmp")
  (subpath "/private/var/tmp")
)
(allow file-ioctl (subpath "/dev"))
(allow sysctl-read)
(allow mach-lookup)
(allow iokit-open)
` + network + `
`
}

func escapeSeatbeltString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
