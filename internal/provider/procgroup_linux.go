//go:build linux

package provider

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts a CLI agent in its own process group, so killing it
// takes its children (MCP servers, shells it spawned) too, and asks the kernel
// to kill it if the daemon dies, however it dies.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
