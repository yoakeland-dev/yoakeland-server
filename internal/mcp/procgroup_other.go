//go:build !linux

package mcp

import "os/exec"

// ponytail: no process-group kill off Linux; a server's grandchildren can outlive a kill.
func ownProcessGroup(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
