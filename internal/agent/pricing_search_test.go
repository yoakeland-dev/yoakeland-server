package agent

import (
	"testing"

	"yoakeland/internal/protocol"
)

// Server-side web searches are billed per request on top of the tokens, so a
// search-heavy turn counts against the cap.
func TestPriceUsageAddsWebSearches(t *testing.T) {
	u := protocol.UsageData{InputTokens: 1_000_000, WebSearches: 3}
	priceUsage("claude-3-5-sonnet-20241022", &u)
	if want := 3.0 + 3*webSearchUSD; !u.CostKnown || u.CostUSD != want {
		t.Fatalf("cost = %v known=%v, want %v", u.CostUSD, u.CostKnown, want)
	}
}
