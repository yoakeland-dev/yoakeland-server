//go:build unix

package provider

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Killing a CLI agent must take the processes it spawned with it.
func TestKillTakesGrandchildren(t *testing.T) {
	p, err := startProc(context.Background(), "sh", []string{"-c", "sleep 300 & echo $!; wait"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !p.lines.Scan() {
		t.Fatal("no pid printed")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(p.lines.Text()))
	if err != nil {
		t.Fatal(err)
	}
	p.kill()
	_ = p.wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// The orphan is reparented and reaped by init; signal 0 fails once it is gone.
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived the kill", pid)
}

// A line longer than the frame limit stops the reader while the CLI is still
// writing. wait must kill the CLI, or it blocks on the full pipe forever.
func TestWaitKillsCLIWhenStdoutOverflows(t *testing.T) {
	p, err := startProc(context.Background(), "sh", []string{"-c", "head -c 12000000 /dev/zero | tr '\\0' x; sleep 60"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.lines.Scan() {
		t.Fatal("expected the over-long line to stop the scanner")
	}
	done := make(chan struct{})
	go func() {
		_ = p.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wait blocked: the CLI was never killed")
	}
}
