//go:build unix

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// setupLocalListener opens the always-on local listener: a unix socket here,
// a named pipe on Windows (socketdir_windows.go).
func setupLocalListener(path string, ownDir bool) (net.Listener, error) {
	return setupUnixListener(path, ownDir)
}

// securePrivateDir checks dir is a real directory (not a symlink) owned by
// this user, and tightens it to 0700 if group/others can get in.
func securePrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket directory %s isn't a directory (a symlink?); refusing to use it", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket directory %s is owned by uid %d, not this user; refusing to use it", dir, st.Uid)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("tightening socket directory %s: %w", dir, err)
		}
		logWarn("socket directory %s was mode %o; tightened to 0700", dir, perm)
	}
	return nil
}

// setupUnixListener binds a unix domain socket at path, creating its parent
// directory (0700) if missing, then chmods the socket file itself 0600 — its
// owner-only permission is the whole auth boundary for connections through
// it, so no token or handshake is needed (see SPEC.md §2a).
//
// ownDir says the parent is a directory yoakeland manages (the default
// runtime/state dir, not a -listen unix:<path> override): it must then be
// ours and private, and is tightened to 0700 if it isn't. An override's
// parent (e.g. /tmp) is left alone; the socket's own 0600 still holds, and
// the moment between bind and chmod is covered by connect() needing write
// permission, which a normal umask doesn't grant anyone else.
//
// A leftover socket from a crashed run is removed, but only after checking
// nothing answers on it — otherwise a second instance would silently take
// the path over from a live one — and only if it really is a socket.
func setupUnixListener(path string, ownDir bool) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating directory for unix socket: %w", err)
	}
	if ownDir {
		if err := securePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and isn't a socket; refusing to replace it", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another yoakeland is already listening on %s", path)
		}
		_ = os.Remove(path) // stale: left by a run that crashed
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("setting permissions on unix socket: %w", err)
	}
	logInfoDetail(
		fmt.Sprintf("local socket listening at %s", path),
		fmt.Sprintf("yoakeland local socket listening at %s (mode 0600 — no auth token needed, owner-only file permissions are the auth boundary)", path),
	)
	return ln, nil
}
