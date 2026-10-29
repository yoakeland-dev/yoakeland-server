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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"yoakeland/internal/normalizer"
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

	timeout := execTimeout(s.timeout)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// --new-session (severs any inherited controlling tty) and network
	// isolation both apply to the agent-issued one-shot command — see
	// buildCmd's doc comment for why SpawnPTY's interactive terminal gets
	// neither.
	stdout, stderr := newCapped(), newCapped()
	build := func() *exec.Cmd {
		cmd := s.buildCmd(execCtx, argv, "", true, false)
		s.setEnv(cmd, s.baseEnv(extraEnv, false))
		cmd.WaitDelay = 2 * time.Second // a descendant holding the pipes can't hang us past the timeout
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return cmd
	}

	// The same memory/pids caps as a terminal, applied from the start.
	leaf := newCgroupLeaf()
	cmd := build()
	leaf.place(cmd)
	err := cmd.Start()
	leaf.release()
	if err != nil && leaf != nil {
		// A start refused inside the leaf runs the command without caps,
		// rather than not at all.
		log.Printf("WARNING: could not start a command in its cgroup (%v); running it without memory and process caps", err)
		leaf.remove()
		leaf = nil
		cmd = build()
		err = cmd.Start()
	}
	if err == nil {
		err = cmd.Wait()
	}
	leaf.remove()

	// Prioritize context deadline / cancellation errors
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

// buildCmd constructs the not-yet-started *exec.Cmd for argv under this
// sandbox's tier, shared by Execute (buffered, one-shot) and SpawnPTY
// (interactive) — same bind mounts, same namespace flags, same project-root
// chdir either way. newSession and allowNetwork differ by caller:
//
// Execute always passes newSession=true: bwrap's --new-session puts the
// sandboxed command in its own terminal session with no controlling tty
// inherited from yoakeland's own, closing off the TIOCSTI ioctl trick
// (pushing synthetic keystrokes onto a terminal you don't own) — a
// well-documented bwrap/container escape technique, the same class of issue
// Flatpak's own bwrap invocation guards against.
//
// SpawnPTY always passes newSession=false: the whole point of a terminal is
// an interactive shell with real job control (Ctrl-Z, Ctrl-C process-group
// signaling), which needs a controlling tty — and the one it gets is the
// freshly allocated pty slave the caller (the terminal's own user) is
// already driving, not yoakeland's own terminal. TIOCSTI there has nothing
// to escalate into: injecting keystrokes into a pty you already have full
// read/write access to isn't a privilege gain.
//
// Execute always passes allowNetwork=false (bwrap's --unshare-net) — a
// deliberate boundary on what an agent-issued command can reach unprompted.
// SpawnPTY passes its caller's choice: true for a human's own terminal,
// opened on purpose, meant to run anything a normal terminal would — git
// clone, curl, npm install, ssh. An agent's terminal gets the network only
// while the project's web tools are on (terminal.Manager.SetAgentNetwork):
// otherwise terminal_start would quietly undo Execute's --unshare-net. The
// same trade-off Codex makes with workspace-write's network_access switch —
// off also means a dev server the agent starts can't be reached from the
// host, since it listens inside its own network namespace.
func (s *LinuxSandbox) buildCmd(ctx context.Context, argv []string, dir string, newSession, allowNetwork bool) *exec.Cmd {
	if dir == "" {
		dir = s.projectRoot
	}
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
		)
		// Git runs these as code whenever it runs outside the sandbox
		// (yoakeland's own git status, the user's commits), so the sandbox
		// may write the project but not them. Must follow the bind above.
		// Submodules keep theirs under .git/modules/<name>/ (git status
		// recurses into them). ponytail: a .git created mid-command, and
		// submodules named with a slash or nested (.git/modules/a/b/,
		// .git/modules/a/modules/b/), stay writable; hostGit disables
		// fsmonitor/hooks for those.
		// Bind .git (and each module dir) onto itself first: a mountpoint
		// can't be renamed, so the sandbox can't mv .git aside and recreate
		// it without the read-only binds below. --bind-try skips a missing
		// .git and binds a worktree/submodule .git file as a file.
		g := filepath.Join(s.projectRoot, ".git")
		bwrapArgs = append(bwrapArgs, "--bind-try", g, g)
		gitPaths := []string{".git/config", ".git/hooks"}
		if mods, err := os.ReadDir(filepath.Join(g, "modules")); err == nil {
			for _, m := range mods {
				md := filepath.Join(g, "modules", m.Name())
				bwrapArgs = append(bwrapArgs, "--bind-try", md, md)
				gitPaths = append(gitPaths, filepath.Join(".git", "modules", m.Name(), "config"), filepath.Join(".git", "modules", m.Name(), "hooks"))
			}
		}
		for _, p := range gitPaths {
			p = filepath.Join(s.projectRoot, p)
			bwrapArgs = append(bwrapArgs, "--ro-bind-try", p, p)
		}
		// yoakeland's own state (tokens, CA key, provider keys) when the
		// project contains it, e.g. a project opened at $HOME.
		for _, h := range normalizer.HiddenIn(s.projectRoot) {
			bwrapArgs = append(bwrapArgs, "--tmpfs", h)
		}
		bwrapArgs = append(bwrapArgs,
			"--chdir", dir,
			"--unshare-pid",
			"--unshare-ipc",
			"--unshare-uts",
			"--die-with-parent",
			// ponytail: namespace/mount isolation only — no seccomp filter;
			// add one if agents start running untrusted code.
			// Execute and SpawnPTY both apply memory/pids cgroup v2 limits
			// when this process has delegation for one (newCgroupLeaf,
			// cgroup_linux.go). Delegation is often unavailable on a bare
			// daemon with no session manager above it (logged once, not
			// silent) — upgrade path there is running under a systemd user
			// session, or granting cgroup delegation another way.
		)
		if newSession {
			bwrapArgs = append(bwrapArgs, "--new-session")
		} else {
			// The PTY path: the shell's --rcfile lives here, after the /tmp tmpfs above.
			if dir := ShellIntegrationDir(); dir != "" {
				bwrapArgs = append(bwrapArgs, "--ro-bind-try", dir, dir)
			}
		}
		if !allowNetwork {
			bwrapArgs = append(bwrapArgs, "--unshare-net")
		}
		bwrapArgs = append(bwrapArgs, "--")
		bwrapArgs = append(bwrapArgs, execArgv...)
		cmd = exec.CommandContext(ctx, "bwrap", bwrapArgs...)
	} else if s.mode == ModeNamespace {
		cmd = exec.CommandContext(ctx, execArgv[0], execArgv[1:]...)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		}
	} else {
		cmd = exec.CommandContext(ctx, execArgv[0], execArgv[1:]...)
		cmd.Dir = dir
	}

	return cmd
}

// setEnv gives the command inside the sandbox env. Under bwrap it goes in as --setenv, so a
// peer-chosen LD_PRELOAD or the like applies to the sandboxed process; as bwrap's own
// environment it would load a library into bwrap on the host, before the sandbox exists.
func (s *LinuxSandbox) setEnv(cmd *exec.Cmd, env []string) {
	if s.mode != ModeBubblewrap {
		cmd.Env = env
		return
	}
	args := []string{cmd.Args[0], "--clearenv"}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, "--setenv", k, v)
	}
	cmd.Args = append(args, cmd.Args[1:]...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
}

// baseEnv is the sandbox's fixed environment, plus extraEnv appended as-is.
// interactive adds TERM, which a captured one-shot command has no use for
// but an interactive shell (readline, ncurses tools) needs to behave sanely.
func (s *LinuxSandbox) baseEnv(extraEnv []string, interactive bool) []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
		"HOME=" + s.projectRoot,
		"PWD=" + s.projectRoot,
	}
	if interactive {
		env = append(env, "TERM=xterm-256color")
	}
	return append(env, extraEnv...)
}

// SpawnPTY starts argv attached to a pty under this sandbox's tier — see
// buildCmd for why --new-session is skipped here specifically. There is no
// enclosing timeout (unlike Execute): ctx cancellation still tears the
// process down (exec.CommandContext), but an interactive session is
// expected to outlive any single request.
func (s *LinuxSandbox) SpawnPTY(ctx context.Context, argv []string, extraEnv []string, dir string, cols, rows int, allowNetwork bool) (PTYSession, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	ws := &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
	build := func() *exec.Cmd {
		cmd := s.buildCmd(ctx, argv, dir, false, allowNetwork)
		s.setEnv(cmd, s.baseEnv(extraEnv, true))
		return cmd
	}

	// Confines the whole spawned tree (bwrap and everything inside its pid
	// namespace, or the shell directly in namespace/local mode — cgroup
	// membership is inherited by descendants regardless of pid-namespace
	// boundaries) to a memory/pids-capped cgroup when this process has
	// delegation for one. Placed before the start, see cgroupLeaf.
	leaf := newCgroupLeaf()
	cmd := build()
	leaf.place(cmd)
	ptmx, err := pty.StartWithSize(cmd, ws)
	leaf.release()
	if err != nil && leaf != nil {
		log.Printf("WARNING: could not start a terminal in its cgroup (%v); running it without memory and process caps", err)
		leaf.remove()
		leaf = nil
		cmd = build()
		ptmx, err = pty.StartWithSize(cmd, ws)
	}
	if err != nil {
		return nil, fmt.Errorf("pty spawn error: %w", err)
	}

	return &linuxPTYSession{cmd: cmd, ptmx: ptmx, cgroupCleanup: leaf.remove, cgroupKill: leaf.kill}, nil
}

// linuxPTYSession implements PTYSession over creack/pty's master file —
// same exit-code extraction convention as Execute.
type linuxPTYSession struct {
	cmd           *exec.Cmd
	ptmx          *os.File
	cgroupCleanup func() // removes this session's cgroup once it's empty; nil-safe no-op if limits weren't applied
	cgroupKill    func() // kills everything left in the cgroup; nil-safe no-op if limits weren't applied
	waited        atomic.Bool
}

func (p *linuxPTYSession) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *linuxPTYSession) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p *linuxPTYSession) Pid() int                    { return p.cmd.Process.Pid }

func (p *linuxPTYSession) Resize(cols, rows int) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (p *linuxPTYSession) Close() error {
	// Closing the master delivers SIGHUP to the slave's foreground process
	// group first (standard tty semantics, same as closing a real terminal
	// window); Kill is a backstop for a process that ignores it.
	err := p.ptmx.Close()
	if p.cmd.Process != nil {
		// Not once the shell has been reaped: its pid could be anyone's by then. Job control gives
		// each background job its own group, so the tree is walked rather than signalled as a group.
		if !p.waited.Load() {
			for _, pid := range descendants(p.cmd.Process.Pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = p.cmd.Process.Kill()
	}
	// Whatever escaped the group (setsid, daemonized): the cgroup holds every descendant.
	if p.cgroupKill != nil {
		p.cgroupKill()
	}
	return err
}

// descendants lists every process below root, found through /proc's parent links. Whatever has
// already been orphaned to init is out of reach here (the cgroup kill covers it where there is one).
func descendants(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// "pid (comm) S ppid ...", and comm may hold spaces and parens.
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			kids[ppid] = append(kids[ppid], pid)
		}
	}
	var out []int
	for queue := kids[root]; len(queue) > 0; queue = queue[1:] {
		out = append(out, queue[0])
		queue = append(queue, kids[queue[0]]...)
	}
	return out
}

func (p *linuxPTYSession) Wait() (int, error) {
	err := p.cmd.Wait()
	p.waited.Store(true)
	if p.cgroupCleanup != nil {
		p.cgroupCleanup()
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
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

	// A second attempt with a longer deadline: a cold cache or a loaded host
	// can push one run past a second, and a false "no bubblewrap" leaves the
	// project unisolated until the daemon restarts. The failure is logged,
	// since the reason (no userns permission, say) is what the user needs.
	for _, timeout := range []time.Duration{time.Second, 5 * time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, "bwrap", args...)
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.WaitDelay = 500 * time.Millisecond // a child holding the pipe can't stall past the deadline
		err := cmd.Run()
		cancel()
		if err == nil {
			return true
		}
		log.Printf("bubblewrap probe failed: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return false
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
		log.Printf("WARNING: could not determine bubblewrap version (%v). Versions before 0.12.0 have known sandbox-escape CVEs (GHSA-pxhw-h44j-8pfx, GHSA-xq78-7hw4-5jvp). Upgrade bubblewrap.", err)
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
		log.Printf("WARNING: bubblewrap %q has known sandbox-escape CVEs: GHSA-pxhw-h44j-8pfx (symlink escape, fixed in 0.12.0) and GHSA-xq78-7hw4-5jvp (ptrace privilege escalation in setuid installs, fixed in 0.11.2). Upgrade bubblewrap.", strings.TrimSpace(string(out)))
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
