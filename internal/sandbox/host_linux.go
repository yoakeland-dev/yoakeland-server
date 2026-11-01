package sandbox

import "syscall"

// killTree ends everything below pid: job control gives each background job its own process
// group, so a signal to the shell's group would miss them.
func killTree(pid int) {
	for _, p := range descendants(pid) {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}
