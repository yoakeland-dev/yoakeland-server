//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockServer takes an advisory lock that every yoakeland server of this user shares (stable and
// early have different state dirs, so it lives in the user cache dir). It is held until the
// returned file is closed or the process dies. A lock it cannot take for any reason but a holder
// is not an error: a read-only cache dir should not stop the server starting.
func lockServer() (*os.File, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, nil
	}
	dir = filepath.Join(dir, "yoake")
	if os.MkdirAll(dir, 0o700) != nil {
		return nil, nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another Yoakeland server is already running (stable and early can't run together); stop it first")
		}
		return nil, nil
	}
	return f, nil
}
