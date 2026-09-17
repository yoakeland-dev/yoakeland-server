package sandbox_test

import (
	"context"
	"testing"
	"time"
	"yoakeland/internal/sandbox"
)

func TestSandbox_ExecutionAndTimeout(t *testing.T) {
	sb := sandbox.NewSandbox(t.TempDir(), 1*time.Second)

	// Quick execution
	res, err := sb.Execute(context.Background(), []string{"echo", "sandbox active"}, nil)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}

	// Timeout enforcement
	ctx := context.Background()
	_, err = sb.Execute(ctx, []string{"sleep", "2"}, nil)
	if err == nil {
		t.Fatalf("expected timeout error for 2-second sleep in 1-second sandbox")
	}
}
