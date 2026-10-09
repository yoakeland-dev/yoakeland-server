package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 5}
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := c.Write([]byte("defgh")); n != 5 || err != nil {
		t.Fatal("Write must report everything consumed", n, err)
	}
	got := c.String()
	if !strings.HasPrefix(got, "abcde\n[output truncated, 3 bytes dropped]") {
		t.Fatalf("got %q", got)
	}
	if small := (&cappedBuffer{max: 5}); small.String() != "" {
		t.Fatal("empty buffer should be empty")
	}
}

// A command that floods stdout is cut at the runtime cap instead of filling memory.
func TestExecuteCapsFloodingCommand(t *testing.T) {
	SetLimits(Limits{ExecTimeout: 2 * time.Second, ExecOutputMaxBytes: 4096})
	defer SetLimits(Limits{})
	sb := NewSandbox(t.TempDir(), 60*time.Second)
	res, err := sb.Execute(context.Background(), []string{"sh", "-c", "yes"}, nil)
	if err == nil {
		t.Fatalf("yes should hit the 2s timeout, got exit %d", res.ExitCode)
	}
	// Timed out: the cap is proven by a bounded run that finishes on its own.
	res, err = sb.Execute(context.Background(), []string{"sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x"}, nil)
	if err != nil {
		t.Skipf("sandbox can't run sh here: %v", err)
	}
	if len(res.Stdout) > 4096+64 || !strings.Contains(res.Stdout, "output truncated") {
		t.Fatalf("stdout len %d, want capped near 4096", len(res.Stdout))
	}
}
