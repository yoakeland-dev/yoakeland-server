// Package fsutil holds the filesystem helpers the daemon's state stores share.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data, so a crash leaves the old file or
// the new one, never a truncated mix. The temp file is created in the same
// directory (rename is only atomic within one filesystem) under a unique name,
// so concurrent writers can't trample each other's temp file. perm is applied
// exactly, not filtered through the umask.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return err
	}
	// Persist the rename itself. Best-effort: not every platform lets a
	// directory be fsynced.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
