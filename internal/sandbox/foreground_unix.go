//go:build linux || darwin

package sandbox

import (
	"os"

	"golang.org/x/sys/unix"
)

// ttyForeground is tcgetpgrp on a pty master. Through SyscallConn rather than Fd(), which would
// switch the file to blocking mode and stop Close from interrupting the pump's Read.
func ttyForeground(f *os.File) (int, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, false
	}
	pg := 0
	var ioerr error
	if err := rc.Control(func(fd uintptr) { pg, ioerr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP) }); err != nil || ioerr != nil {
		return 0, false
	}
	return pg, pg > 0
}
