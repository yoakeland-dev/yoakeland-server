package provider

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pricingURL is LiteLLM's community-maintained per-model rate table (MIT
// licensed, public repo, no auth needed) — the same source ccusage and other
// agent-harness cost trackers price against, rather than a hand-maintained
// map that inevitably drifts out of date as new models ship.
const pricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// pricingFallback is a last-known-good snapshot baked into the binary, so a
// fresh install that has never successfully fetched still prices current
// models instead of falling back straight to pricing.go's tiny static table.
//
//go:embed pricing_fallback.json
var pricingFallback []byte

// pricingRate is USD per single token (LiteLLM's own unit — pricing.go's
// rate type is USD per million, converted at lookup time in EstimateCostUSD).
type pricingRate struct {
	InputPerToken  float64
	OutputPerToken float64
	// Prompt-cache rates; 0 when LiteLLM doesn't list one.
	CacheReadPerToken  float64
	CacheWritePerToken float64
}

// pricingCacheFile mirrors manifestCacheFile: the raw upstream bytes plus
// the ETag that produced them, so a restart resumes conditional-GETing
// instead of re-fetching the ~2.5MB file.
type pricingCacheFile struct {
	ETag      string          `json:"etag"`
	FetchedAt time.Time       `json:"fetchedAt"`
	Raw       json.RawMessage `json:"raw"`
}

// PricingCache serves live per-model USD rates for EstimateCostUSD (see
// pricing.go), fetched from LiteLLM's model_prices_and_context_window.json.
// Lookup never blocks on the network — Refresh runs only in the background
// (StartDailyRefresh) and every read is served from whatever was last parsed
// successfully, all the way down to the embedded fallback if this server has
// never fetched at all. Modeled directly on ManifestCache (manifest.go),
// which solves the identical problem for cli_claude/cli_codex's model list.
type PricingCache struct {
	mu         sync.RWMutex
	cachePath  string // "" disables disk persistence (used by tests)
	url        string // overridable by tests; defaults to pricingURL
	httpClient *http.Client

	etag       string
	rates      map[string]pricingRate
	failStreak int
}

// NewPricingCache loads <stateDir>/pricing-cache.json if present, falling
// back to the embedded snapshot otherwise (or on a corrupt cache file). It
// never fetches over the network — call Refresh or StartDailyRefresh.
func NewPricingCache(stateDir string) *PricingCache {
	pc := &PricingCache{url: pricingURL, httpClient: &http.Client{Timeout: 20 * time.Second}}
	if stateDir != "" {
		pc.cachePath = filepath.Join(stateDir, "pricing-cache.json")
	}

	raw := pricingFallback
	etag := ""
	if pc.cachePath != "" {
		if data, err := os.ReadFile(pc.cachePath); err == nil {
			var cached pricingCacheFile
			if json.Unmarshal(data, &cached) == nil && len(cached.Raw) > 0 {
				raw, etag = cached.Raw, cached.ETag
			}
		}
	}

	rates, err := parsePricing(raw)
	if err != nil {
		log.Printf("pricing table: embedded/cached fallback failed to parse: %v", err)
	}
	pc.rates, pc.etag = rates, etag
	return pc
}

// Lookup returns model's USD-per-token input/output rate. It tries, in
// order: the exact model string as given; "openrouter/" + model (OpenRouter
// providers pass yoakeland's own model string straight through as e.g.
// "anthropic/claude-3.5-sonnet", which is exactly the suffix LiteLLM keys
// its openrouter/* entries by); and the bare name after the last "/" (a
// provider-qualified slug LiteLLM lists unqualified, or vice versa). Returns
// ok=false rather than a guess when none of those hit.
func (pc *PricingCache) Lookup(model string) (rate pricingRate, ok bool) {
	if pc == nil || model == "" {
		return pricingRate{}, false
	}
	key := strings.ToLower(strings.TrimSpace(model))

	pc.mu.RLock()
	defer pc.mu.RUnlock()

	for _, candidate := range []string{key, "openrouter/" + key, bareModelName(key)} {
		if r, found := pc.rates[candidate]; found {
			return r, true
		}
	}
	return pricingRate{}, false
}

// CostUSD prices one request with prompt-cache reads and writes split out
// (both part of input). A missing cache rate falls back to the input rate,
// so an unknown discount over- rather than under-reports.
func (pc *PricingCache) CostUSD(model string, input, cacheRead, cacheWrite, output int) (float64, bool) {
	r, ok := pc.Lookup(model)
	if !ok {
		return 0, false
	}
	readRate, writeRate := r.CacheReadPerToken, r.CacheWritePerToken
	if readRate == 0 {
		readRate = r.InputPerToken
	}
	if writeRate == 0 {
		writeRate = r.InputPerToken
	}
	plain := input - cacheRead - cacheWrite
	return float64(plain)*r.InputPerToken + float64(cacheRead)*readRate + float64(cacheWrite)*writeRate + float64(output)*r.OutputPerToken, true
}

func bareModelName(key string) string {
	if i := strings.LastIndex(key, "/"); i != -1 {
		return key[i+1:]
	}
	return key
}

// StartDailyRefresh fetches once immediately in the background (best-effort,
// never blocking the caller) and then again at a fixed 00:00 UTC every day
// after. Returns once ctx is done. See ManifestCache.StartDailyRefresh,
// which this mirrors exactly.
func (pc *PricingCache) StartDailyRefresh(ctx context.Context) {
	go func() {
		_ = pc.Refresh()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(nextUTCMidnight(time.Now().UTC()))):
				_ = pc.Refresh()
			}
		}
	}()
}

// Refresh does one conditional GET against pricingURL: If-None-Match with
// whatever ETag the last successful fetch returned, so a day where upstream
// hasn't changed costs a 304 rather than the full ~2.5MB body. On any
// failure it logs and leaves the last-good cache serving exactly as it was.
func (pc *PricingCache) Refresh() error {
	req, err := http.NewRequest(http.MethodGet, pc.url, nil)
	if err != nil {
		return err
	}
	pc.mu.RLock()
	etag := pc.etag
	pc.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := pc.httpClient.Do(req)
	if err != nil {
		pc.recordFailure(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		pc.recordSuccess()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("pricing table fetch: unexpected status %d", resp.StatusCode)
		pc.recordFailure(err)
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		pc.recordFailure(err)
		return err
	}

	rates, err := parsePricing(body)
	if err != nil {
		log.Printf("pricing table: fetched but failed to parse, keeping last-good cache: %v", err)
		pc.recordFailure(err)
		return err
	}

	newETag := resp.Header.Get("ETag")
	pc.mu.Lock()
	pc.rates, pc.etag = rates, newETag
	pc.mu.Unlock()
	pc.recordSuccess()

	if pc.cachePath != "" {
		data, err := json.Marshal(pricingCacheFile{ETag: newETag, FetchedAt: time.Now().UTC(), Raw: body})
		if err == nil {
			tmp := pc.cachePath + ".tmp"
			if err := os.WriteFile(tmp, data, 0644); err == nil {
				_ = os.Rename(tmp, pc.cachePath)
			}
		}
	}
	return nil
}

// recordFailure only warns once a failure has persisted across more than one
// scheduled (daily) attempt — see ManifestCache.recordFailure for the same
// reasoning.
func (pc *PricingCache) recordFailure(err error) {
	pc.mu.Lock()
	pc.failStreak++
	streak := pc.failStreak
	pc.mu.Unlock()
	if streak >= 2 {
		log.Printf("WARNING: pricing table fetch has failed %d times in a row (%v) — still serving the last-good cache", streak, err)
	}
}

func (pc *PricingCache) recordSuccess() {
	pc.mu.Lock()
	pc.failStreak = 0
	pc.mu.Unlock()
}

// rawPricingEntry is only the slice of each LiteLLM entry yoakeland actually
// uses. Extra upstream fields are simply ignored by encoding/json, which is
// what keeps this defensive against upstream reshaping.
type rawPricingEntry struct {
	InputCostPerToken      *float64 `json:"input_cost_per_token"`
	OutputCostPerToken     *float64 `json:"output_cost_per_token"`
	CacheReadCostPerToken  float64  `json:"cache_read_input_token_cost"`
	CacheWriteCostPerToken float64  `json:"cache_creation_input_token_cost"`
}

// parsePricing projects LiteLLM's document into model -> rate. Entries
// missing either an input or an output rate are dropped: a half-priced
// model would silently under-report cost, which is worse than reporting the
// model as unpriced (EstimateCostUSD's static table, or $0, takes over).
func parsePricing(raw []byte) (map[string]pricingRate, error) {
	var doc map[string]rawPricingEntry
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decoding pricing table: %w", err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("pricing table has no entries")
	}

	rates := make(map[string]pricingRate, len(doc))
	for name, entry := range doc {
		if entry.InputCostPerToken == nil || entry.OutputCostPerToken == nil {
			continue
		}
		rates[strings.ToLower(name)] = pricingRate{
			InputPerToken:      *entry.InputCostPerToken,
			OutputPerToken:     *entry.OutputCostPerToken,
			CacheReadPerToken:  entry.CacheReadCostPerToken,
			CacheWritePerToken: entry.CacheWriteCostPerToken,
		}
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("pricing table has no entries with both input and output rates")
	}
	return rates, nil
}
