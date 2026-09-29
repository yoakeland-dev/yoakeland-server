package provider

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live: asks the real claude and codex on this machine for their models (YK_LIVE=1).
func TestLiveDiscovery(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cl, err := discoverClaudeModels(ctx, t.TempDir())
	if err != nil || len(cl) == 0 {
		t.Fatalf("claude: %v (%d models)", err, len(cl))
	}
	for _, m := range cl {
		t.Logf("claude  %-18s %-24s %v", m.ID, m.DisplayName, m.Capabilities)
	}
	cx, err := discoverCodexModels(ctx, t.TempDir())
	if err != nil || len(cx) == 0 {
		t.Fatalf("codex: %v (%d models)", err, len(cx))
	}
	for _, m := range cx {
		t.Logf("codex   %-18s %-24s %v", m.ID, m.DisplayName, m.Capabilities)
	}
}
