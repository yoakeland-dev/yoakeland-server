//go:build unix

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockProject takes an advisory lock on a project root that every server of this user shares
// (state dirs differ between stable and early, so the lock lives in the user cache dir). It is
// held until the returned file is closed or the process dies. A lock it cannot take for any
// reason but a holder is not an error: a read-only cache dir should not stop a project opening.
func lockProject(key string) (*os.File, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, nil
	}
	dir = filepath.Join(dir, "yoake", "locks")
	if os.MkdirAll(dir, 0o700) != nil {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(key))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:8])), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("project %s is open in another Yoakeland server (stable and early can't share a project); close it there first", key)
		}
		return nil, nil
	}
	return f, nil
}
