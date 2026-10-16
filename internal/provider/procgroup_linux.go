//go:build linux

package provider

import "syscall"

// setParentDeathSignal asks the kernel to kill a CLI agent if the daemon dies,
// however it dies.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
