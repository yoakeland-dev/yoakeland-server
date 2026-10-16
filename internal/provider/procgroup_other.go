//go:build !unix

package provider

import "os/exec"

// Windows has no process groups here: a kill reaches only the CLI itself.
func ownProcessGroup(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
