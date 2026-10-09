//go:build !linux

package provider

import "os/exec"

// ponytail: no process-group kill off Linux; grandchildren can outlive a kill.
func ownProcessGroup(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
