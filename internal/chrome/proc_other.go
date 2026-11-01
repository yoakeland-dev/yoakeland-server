//go:build !linux

package chrome

import "os/exec"

func confine(*exec.Cmd) {}
