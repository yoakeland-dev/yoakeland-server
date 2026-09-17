//go:build linux || darwin

package tool

import (
	"os"
	"syscall"
)

// openNoFollow opens path with flag|O_NOFOLLOW, so a symlink planted at path
// between an earlier path-containment check (normalizer.ResolveAndValidatePath)
// and this call is refused (ELOOP) rather than silently followed and written
// through to wherever it points, possibly outside the project root.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}
