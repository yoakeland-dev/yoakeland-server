package chrome

import (
	"os/exec"
	"syscall"
)

// ponytail: Pdeathsig fires when the launching OS thread exits, not the process; the Go runtime
// keeps its threads, and EOF on the debugging pipe ends Chrome too, so this is only a backstop.
func confine(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
