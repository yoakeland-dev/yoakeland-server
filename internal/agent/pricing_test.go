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

func TestEstimateCost_KnownFlag(t *testing.T) {
	usage := protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if usd, known := agent.EstimateCost("claude-3-5-sonnet-20241022", usage); !known || usd != 18 {
		t.Errorf("listed model: got %v, %v", usd, known)
	}
	if usd, known := agent.EstimateCost("some-unlisted-local-model", usage); known || usd != 0 {
		t.Errorf("unlisted model must report known=false, got %v, %v", usd, known)
	}
	if _, known := agent.EstimateCost("", usage); known {
		t.Error("no model must report known=false")
	}
}

// TestEstimateCostUSD_PrefersLiveSourceOverStaticTable guards the wiring
// SetLivePricingSource exists for: a live-fetched rate (internal/provider's
// PricingCache, current as of the last daily refresh) must win over the
// static table's hand-maintained, inevitably-stale numbers — and a model
// the live source has never heard of must still fall back to the static
// table rather than pricing at $0.
func TestEstimateCostUSD_PrefersLiveSourceOverStaticTable(t *testing.T) {
	t.Cleanup(func() { agent.SetLivePricingSource(nil) })

	agent.SetLivePricingSource(func(model string, u protocol.UsageData) (float64, bool) {
		if model == "claude-3-5-sonnet-20241022" {
			// Deliberately different from the static table's $3/$15 per
			// million, so a test failure here can only mean the static
			// table was used instead of this one.
			return float64(u.InputTokens)*0.000001 + float64(u.OutputTokens)*0.000002, true
		}
		return 0, false
	})

	usage := protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	if got, want := agent.EstimateCostUSD("claude-3-5-sonnet-20241022", usage), 1.0+2.0; got != want {
		t.Errorf("live-priced model: got cost %v, want %v (the live rate, not the static table's)", got, want)
	}

	// A model the live source doesn't know still falls back to the static
	// table rather than pricing at $0 just because a live source exists.
	if got, want := agent.EstimateCostUSD("gpt-4o", usage), 2.5+10.0; got != want {
		t.Errorf("live source miss: got cost %v, want %v (the static table's rate)", got, want)
	}
}
