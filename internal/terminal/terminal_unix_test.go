//go:build unix

package terminal_test

import (
	"bytes"
	"context"
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
