//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// A spawned terminal session's resource caps come from Limits, applied via a
// Job Object — the closest Windows analogue to cgroup_linux.go's cgroup v2 limits.

// WindowsSandbox has no bubblewrap-equivalent filesystem/namespace isolation
// — Windows has no single well-known tool that plays bubblewrap's role, and
// building one (an AppContainer profile, or a full Windows Sandbox VM) is
// out of scope here. Mode() reports this honestly as ModeLocalRestr, the
// same "no real isolation" label the Linux tier uses when its own better
// options are unavailable. SpawnPTY still applies a Job Object resource cap
// (see applyJobObjectLimits) — real, but a narrower guarantee than what
// ModeBubblewrap/ModeSeatbelt mean elsewhere, which is exactly why it isn't
// given its own SandboxMode value.
//
// This has not been run on real Windows — this project's development
// happens on Linux. ConPTY (via github.com/UserExistsError/conpty) and Job
// Objects are both used here through documented Win32 APIs and their
// existing golang.org/x/sys/windows bindings rather than hand-rolled
// syscalls, to keep the unverified surface area as small as possible.
type WindowsSandbox struct {
	projectRoot string
	timeout     time.Duration
}

func NewSandbox(projectRoot string, defaultTimeout time.Duration) Sandbox {
	return &WindowsSandbox{projectRoot: projectRoot, timeout: defaultTimeout}
}

func (s *WindowsSandbox) Mode() SandboxMode {
	return ModeLocalRestr
}

// windowsBaseEnv builds a minimal, explicit environment rather than
// inheriting this daemon's own (which could leak provider API keys etc. into
// a sandboxed process) — same rationale as the Linux/Darwin tiers' baseEnv,
// just with Windows-shaped variables (SystemRoot/ComSpec/USERPROFILE, not
// PATH=/usr/bin:...) since the POSIX defaults elsewhere in this package
// would simply not resolve anything on Windows.
func windowsBaseEnv(projectRoot string, extraEnv []string) []string {
	env := []string{
		"SystemRoot=C:\\Windows",
		"PATH=C:\\Windows\\System32;C:\\Windows;C:\\Windows\\System32\\WindowsPowerShell\\v1.0",
		"ComSpec=C:\\Windows\\System32\\cmd.exe",
		"TEMP=" + projectRoot,
		"TMP=" + projectRoot,
		"USERPROFILE=" + projectRoot,
	}
	return append(env, extraEnv...)
}

func (s *WindowsSandbox) Execute(ctx context.Context, argv []string, extraEnv []string) (*ExecutionResult, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	timeout := execTimeout(s.timeout)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.Dir = s.projectRoot
	cmd.Env = windowsBaseEnv(s.projectRoot, extraEnv)
	cmd.WaitDelay = 2 * time.Second // a descendant holding the pipes can't hang us past the timeout

	stdout, stderr := newCapped(), newCapped()
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	if execCtx.Err() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("execution timed out after %v: %w", timeout, execCtx.Err())
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

// SpawnPTY starts argv attached to a Windows ConPTY pseudo console — the
// Windows equivalent of a POSIX pty. creack/pty (used on Linux/macOS/BSD)
// does NOT implement this on Windows: its Windows build tags exist only to
// keep the package compiling there, and its StartWithSize is a hard-coded
// ErrUnsupported stub. This tier uses a dedicated ConPTY library instead of
// the shared pty.StartWithSize path the other platforms use.
func (s *WindowsSandbox) SpawnPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int, allowNetwork bool) (PTYSession, error) {
	if dir == "" {
		dir = s.projectRoot
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = syscall.EscapeArg(a)
	}
	commandLine := strings.Join(parts, " ")

	cp, err := conpty.Start(
		commandLine,
		conpty.ConPtyDimensions(cols, rows),
		conpty.ConPtyWorkDir(dir),
		conpty.ConPtyEnv(windowsBaseEnv(s.projectRoot, extraEnv)),
	)
	if err != nil {
		return nil, fmt.Errorf("conpty spawn error: %w", err)
	}

	jobCleanup, _ := applyJobObjectLimits(cp.Pid())

	sess := &windowsPTYSession{cpty: cp, jobCleanup: jobCleanup}
	// conpty.Start takes no context itself (unlike exec.CommandContext on
	// the other platforms) — this is what makes ctx cancellation actually
	// tear the session down instead of being silently ignored.
	context.AfterFunc(ctx, func() { _ = sess.Close() })
	return sess, nil
}

type windowsPTYSession struct {
	cpty       *conpty.ConPty
	jobCleanup func()
}

func (p *windowsPTYSession) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *windowsPTYSession) Write(b []byte) (int, error) { return p.cpty.Write(b) }
func (p *windowsPTYSession) Pid() int                    { return p.cpty.Pid() }

func (p *windowsPTYSession) Resize(cols, rows int) error {
	return p.cpty.Resize(cols, rows)
}

func (p *windowsPTYSession) Close() error {
	// Backstop kill before releasing handles: ClosePseudoConsole (inside
	// cpty.Close) tears down the pipes the shell reads/writes, which is
	// usually enough to make it exit on its own — the ConPTY analogue of
	// SIGHUP on a closed pty master — but isn't a guaranteed termination the
	// way it is on POSIX, so this doesn't rely on that alone.
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.cpty.Pid())); err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
	return p.cpty.Close()
}

func (p *windowsPTYSession) Wait() (int, error) {
	exitCode, err := p.cpty.Wait(context.Background())
	if p.jobCleanup != nil {
		p.jobCleanup()
	}
	if err != nil {
		return -1, err
	}
	return int(exitCode), nil
}

// applyJobObjectLimits confines pid to a Windows Job Object capping memory
// and active-process count — the Job Object equivalent of
// cgroup_linux.go's newCgroupLeaf. Every step's error is checked, same
// "don't claim success on a partial failure" rule that function documents.
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE makes the job handle itself a kill
// switch: closing it (which the OS does automatically if this daemon
// process ever exits without running cleanup) tears down every process
// still in it, so a crashed daemon doesn't leak orphaned shells either.
func applyJobObjectLimits(pid int) (cleanup func(), applied bool) {
	noop := func() {}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return noop, false
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags:         windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			ActiveProcessLimit: uint32(CurrentLimits().TerminalPids),
		},
		ProcessMemoryLimit: uintptr(CurrentLimits().TerminalMemoryBytes),
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return noop, false
	}

	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return noop, false
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return noop, false
	}

	return func() { _ = windows.CloseHandle(job) }, true
}
