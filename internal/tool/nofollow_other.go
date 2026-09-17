//go:build !linux && !darwin

package tool

import "os"

// openNoFollow is the non-Unix fallback: Go's syscall package doesn't expose
// O_NOFOLLOW on platforms outside linux/darwin (notably Windows), and
// symlink creation there normally requires elevated privileges anyway, so
// this just opens the file normally rather than failing to build.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
