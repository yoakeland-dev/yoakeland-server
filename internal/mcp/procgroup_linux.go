//go:build linux

package mcp

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts a server in its own process group, so killing it takes
// the children it spawned too, and has the kernel kill it if the daemon dies,
// however it dies.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
