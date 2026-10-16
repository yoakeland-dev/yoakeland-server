package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewPricingCacheFallsBackToEmbedded(t *testing.T) {
	pc := NewPricingCache("")
	// gpt-4o is a long-lived, unlikely-to-disappear entry in LiteLLM's
	// table, good enough to confirm the embedded snapshot actually parsed.
	if _, ok := pc.Lookup("gpt-4o"); !ok {
		t.Fatalf("expected embedded fallback to price gpt-4o")
	}
}

func TestPricingCacheLookup_OpenRouterAndBareNameFallback(t *testing.T) {
	body := `{
		"gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001},
		"openrouter/anthropic/claude-3.5-sonnet": {"input_cost_per_token": 0.000003, "output_cost_per_token": 0.000015},
		"claude-opus-5": {"input_cost_per_token": 0.000005, "output_cost_per_token": 0.000025}
	}`
	rates, err := parsePricing([]byte(body))
	if err != nil {
		t.Fatalf("parsePricing: %v", err)
	}
	pc := &PricingCache{rates: rates}

	// Exact match.
	if r, ok := pc.Lookup("gpt-4o"); !ok || r.InputPerToken != 0.0000025 {
		t.Errorf("exact match: got %+v, ok=%v", r, ok)
	}
	// yoakeland's OpenRouter provider passes "anthropic/claude-3.5-sonnet"
	// straight through as the model string — LiteLLM keys the same model
	// as "openrouter/anthropic/claude-3.5-sonnet".
	if r, ok := pc.Lookup("anthropic/claude-3.5-sonnet"); !ok || r.OutputPerToken != 0.000015 {
		t.Errorf("openrouter/ prefix match: got %+v, ok=%v", r, ok)
	}
	// Case-insensitive.
	if _, ok := pc.Lookup("GPT-4O"); !ok {
		t.Errorf("expected case-insensitive match")
	}
	// No match anywhere.
	if _, ok := pc.Lookup("some-model-nobody-has-heard-of"); ok {
		t.Errorf("expected no match for an unknown model")
	}
}

func TestParsePricing_DropsEntriesMissingEitherRate(t *testing.T) {
	body := `{
		"priced":       {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000002},
		"input-only":   {"input_cost_per_token": 0.000001},
		"output-only":  {"output_cost_per_token": 0.000002},
		"neither":      {"mode": "chat"}
	}`
	rates, err := parsePricing([]byte(body))
	if err != nil {
		t.Fatalf("parsePricing: %v", err)
	}
	if len(rates) != 1 {
		t.Fatalf("expected only the fully-priced entry to survive, got %+v", rates)
	}
	if _, ok := rates["priced"]; !ok {
		t.Errorf("expected \"priced\" to survive, got %+v", rates)
	}
}

func TestPricingCacheRefreshConditionalGET(t *testing.T) {
	const body = `{"gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001}}`
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	pc := NewPricingCache(dir)
	pc.httpClient = srv.Client()
	pc.url = srv.URL
	pc.rates = nil // the embedded table would make this one-model body a mass drop

	if err := pc.Refresh(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if r, ok := pc.Lookup("gpt-4o"); !ok || r.InputPerToken != 0.0000025 {
		t.Fatalf("unexpected rate after refresh: %+v, ok=%v", r, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "pricing-cache.json")); err != nil {
		t.Errorf("expected on-disk cache file to be written: %v", err)
	}

	if err := pc.Refresh(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}

	// Reload from disk to confirm the cache round-trips.
	pc2 := NewPricingCache(dir)
	if _, ok := pc2.Lookup("gpt-4o"); !ok {
		t.Fatalf("reloaded cache has no rate for gpt-4o")
	}
}

func TestPricingCacheKeepsLastGoodOnMalformedFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	pc := NewPricingCache("")
	pc.httpClient = srv.Client()
	pc.url = srv.URL

	before, ok := pc.Lookup("gpt-4o")
	if !ok {
		t.Fatalf("expected embedded fallback to price gpt-4o before refresh")
	}

	if err := pc.Refresh(); err == nil {
		t.Fatalf("expected malformed fetch to return an error")
	}

	after, ok := pc.Lookup("gpt-4o")
	if !ok || after != before {
		t.Fatalf("expected last-good rate to survive a malformed fetch: before=%+v after=%+v ok=%v", before, after, ok)
	}
}

// A table that drops most models is refused and the last good rates kept; the
// same body served again and again is taken as real. One model zeroed or moved
// tenfold keeps its old rate while the rest of the table is taken, and a model
// getting its first price is taken too.
func TestPricingCacheRefusesImplausibleTable(t *testing.T) {
	pc := &PricingCache{}
	pc.rates = map[string]pricingRate{"free": {}}
	for i := 0; i < 10; i++ {
		pc.rates[fmt.Sprintf("m%d", i)] = pricingRate{InputPerToken: 1e-6, OutputPerToken: 2e-6}
	}

	next := map[string]pricingRate{"free": {InputPerToken: 5e-6, OutputPerToken: 5e-6}}
	for i := 0; i < 10; i++ {
		next[fmt.Sprintf("m%d", i)] = pricingRate{InputPerToken: 2e-6, OutputPerToken: 3e-6}
	}
	next["m0"] = pricingRate{}
	next["m1"] = pricingRate{InputPerToken: 1e-3, OutputPerToken: 2e-6}
	if why := pricingShift(pc.rates, next); why != "" {
		t.Fatalf("pricingShift refused a table with every model present: %s", why)
	}
	if held := holdRepriced(pc.rates, next); fmt.Sprint(held) != "[m0 m1]" {
		t.Fatalf("held = %v, want [m0 m1]", held)
	}
	if next["m0"] != pc.rates["m0"] || next["m1"] != pc.rates["m1"] || next["m2"].InputPerToken != 2e-6 || next["free"].InputPerToken != 5e-6 {
		t.Fatalf("next = %+v", next)
	}

	dropped := `{"m0": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6}}`
	parsed, err := parsePricing([]byte(dropped))
	if err != nil {
		t.Fatalf("parsePricing: %v", err)
	}
	if why := pricingShift(pc.rates, parsed); why == "" {
		t.Fatal("pricingShift accepted a table missing most models")
	}

	// Sustained: the same implausible body eventually counts as real.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(dropped))
	}))
	defer srv.Close()
	pc.httpClient, pc.url = srv.Client(), srv.URL
	for i := 0; i < pricingRefusalsToAccept; i++ {
		if err := pc.Refresh(); err == nil {
			t.Fatalf("refresh %d accepted a gutted table", i+1)
		}
	}
	if err := pc.Refresh(); err != nil {
		t.Fatalf("repeated table still refused: %v", err)
	}
}

// A negative rate is garbage: the entry is dropped rather than priced.
func TestParsePricingDropsNegativeRates(t *testing.T) {
	rates, err := parsePricing([]byte(`{"neg": {"input_cost_per_token": -1, "output_cost_per_token": 1}, "ok": {"input_cost_per_token": 1, "output_cost_per_token": 1}}`))
	if err != nil {
		t.Fatalf("parsePricing: %v", err)
	}
	if _, ok := rates["neg"]; ok || len(rates) != 1 {
		t.Fatalf("rates = %+v, want only \"ok\"", rates)
	}
}
