//go:build linux

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
