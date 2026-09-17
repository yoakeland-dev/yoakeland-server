package sockpath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultStateDirIsOSAppropriate(t *testing.T) {
	dir := DefaultStateDir()
	switch runtime.GOOS {
	case "darwin":
		if want := filepath.Join("Library", "Application Support", "yoake"); !strings.HasSuffix(dir, want) {
			t.Errorf("DefaultStateDir() = %q, want suffix %q", dir, want)
		}
	case "windows":
		if !strings.HasSuffix(dir, `yoake`) || strings.Contains(dir, ".yoake") {
			t.Errorf("DefaultStateDir() = %q, want an AppData\\yoake path, not a dotfile", dir)
		}
	default:
		if !strings.HasSuffix(dir, ".yoake") {
			t.Errorf("DefaultStateDir() = %q, want suffix %q", dir, ".yoake")
		}
	}
}

func TestDefaultRuntimeDirFallsBackToStateDir(t *testing.T) {
	for _, v := range []string{"XDG_RUNTIME_DIR", "TMPDIR"} {
		old, had := os.LookupEnv(v)
		os.Unsetenv(v)
		if had {
			defer os.Setenv(v, old)
		}
	}

	stateDir := "/some/state/dir"
	got := DefaultRuntimeDir(stateDir)
	if runtime.GOOS == "windows" {
		if got == stateDir {
			t.Errorf("DefaultRuntimeDir() = %q, want os.TempDir()-based path on windows even with no env vars set", got)
		}
		return
	}
	if got != stateDir {
		t.Errorf("DefaultRuntimeDir(%q) = %q, want fallback to stateDir with no runtime env vars set", stateDir, got)
	}
}
