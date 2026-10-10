//go:build unix

package terminal_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/terminal"
)

// TestManager_CloseSessionKillsTheShellProcess guards the actual point of
// requirement (1) — "deleting a chat kills them" means the underlying shell
// process itself, not just the bookkeeping entry. Process-liveness-via-
// signal-0 is POSIX-specific, hence the separate build-tagged file (see
// terminal_test.go for the portable coverage).
func TestManager_CloseSessionKillsTheShellProcess(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})

	snap, err := mgr.Open(context.Background(), ipc, "sess-1", "term-1", "", 0, 0, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pid := snap.Pid

	mgr.CloseSession("sess-1")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // process is gone — success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell process (pid %d) still alive after CloseSession", pid)
}

// TestManager_OpenStartsInRequestedCwd: terminal_open's cwd (the explorer's "Open terminal
// here") is where the shell starts, not just what the snapshot reports.
func TestManager_OpenStartsInRequestedCwd(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempDir, "pkg", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatalf("normalizer.New: %v", err)
	}
	mgr := terminal.NewManager(sandbox.NewSandbox(tempDir, 60*time.Second), norm)
	out := &syncBuffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	// HOME is the project root, so bash writes .bash_history there on SIGHUP and can
	// land it while TempDir is being removed.
	if _, err := mgr.Open(context.Background(), ipc, "sess-1", "term-1", "pkg/api", 80, 24, map[string]string{"HISTFILE": "/dev/null"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mgr.Close("sess-1", "term-1")

	if err := mgr.Input("sess-1", "term-1", base64.StdEncoding.EncodeToString([]byte("echo CWD=$(pwd -P)=\n"))); err != nil {
		t.Fatalf("Input: %v", err)
	}
	want := "CWD=" + filepath.Join(tempDir, "pkg", "api") + "="
	if resolved, err := filepath.EvalSymlinks(filepath.Join(tempDir, "pkg", "api")); err == nil {
		want = "CWD=" + resolved + "="
	}
	deadline := time.Now().Add(5 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		if seen = decodedTerminalOutput(t, out.String()); strings.Contains(seen, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell didn't start in pkg/api: want %q in %q", want, seen)
}

// Opening past the cap is refused, so a paired peer can't exhaust pids/memory.
func TestManager_OpenRefusesPastTerminalCap(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})
	defer mgr.CloseSession("sess-cap")

	for i := 0; i < 64; i++ {
		if _, err := mgr.Open(context.Background(), ipc, "sess-cap", fmt.Sprintf("term-%d", i), "", 0, 0, nil); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}
	if _, err := mgr.Open(context.Background(), ipc, "sess-cap", "term-over", "", 0, 0, nil); err == nil {
		t.Fatal("65th terminal opened, want refusal")
	}
}
