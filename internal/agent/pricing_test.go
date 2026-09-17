package agent_test

import (
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

func TestEstimateCostUSD(t *testing.T) {
	usage := protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	got := agent.EstimateCostUSD("claude-3-5-sonnet-20241022", usage)
	want := 3.0 + 15.0 // $3/M input + $15/M output at 1M tokens each
	if got != want {
		t.Fatalf("known model: got cost %v, want %v", got, want)
	}

	if got := agent.EstimateCostUSD("some-unlisted-local-model", usage); got != 0 {
		t.Fatalf("unknown model: got cost %v, want 0", got)
	}
}
