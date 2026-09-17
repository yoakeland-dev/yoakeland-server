// Package sockpath is the single place that defines where yoakeland's
// default local unix socket lives, so the server (which listens there) and
// yoake-client (which dials it) can never disagree about the path.
package sockpath

import (
	"os"
	"path/filepath"
	"runtime"
)

// SocketName is the socket file created under a runtime dir when no
// explicit path is given to -listen/-connect.
const SocketName = "yoakeland.sock"

// DefaultStateDir is where persistent server-level state (providers.json,
// the self-managed TLS CA, logs/) lives by default, in whatever location
// each OS actually expects that kind of data:
//   - macOS: ~/Library/Application Support/yoake — the standard per-app
//     state location; a bare dotfile is a Unix idiom Finder/macOS tooling
//     doesn't expect.
//   - Windows: %AppData%\yoake (roaming app data), via os.UserConfigDir —
//     the Windows equivalent of "app state that should survive a reinstall
//     and roam with the user profile".
//   - everything else (Linux, BSDs, ...): ~/.yoake, the traditional Unix
//     dotfile convention (~/.ssh, ~/.aws, ...).
//
// Falls back to ".yoake" in the working directory if the home/config
// directory can't be determined at all.
func DefaultStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "yoake")
		}
	case "windows":
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "yoake")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".yoake"
	}
	return filepath.Join(home, ".yoake")
}

// DefaultRuntimeDir picks where ephemeral runtime state (currently just the
// unix socket) belongs by default: a socket isn't persistent data, so it
// shouldn't default to living next to providers.json in a directory that
// gets backed up or may sit on a network-mounted home directory. Prefer each
// OS's own per-user ephemeral/temp location:
//   - Linux: $XDG_RUNTIME_DIR (tmpfs, per-user, already 0700 — the same
//     convention ssh-agent and pipewire use)
//   - macOS: $TMPDIR (set per login session by the OS)
//   - Windows: os.TempDir() (%LocalAppData%\Temp — a real per-user temp
//     directory, not roamed or backed up like %AppData%)
//
// and fall back to stateDir otherwise, which covers anything without a
// login session exporting those vars (plain SSH, cron, minimal containers).
func DefaultRuntimeDir(stateDir string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.TempDir(), "yoake")
	case "darwin":
		if dir := os.Getenv("TMPDIR"); dir != "" {
			return filepath.Join(dir, "yoake")
		}
	default:
		if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
			return filepath.Join(dir, "yoake")
		}
	}
	return stateDir
}

// DefaultSocketPath returns <dir>/yoakeland.sock.
func DefaultSocketPath(dir string) string {
	return filepath.Join(dir, SocketName)
}
