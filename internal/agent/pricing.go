package agent

import "yoakeland/internal/protocol"

// rate is USD per 1M tokens.
type rate struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// livePricingSource, when set (see SetLivePricingSource), is consulted
// before the static pricingTable below — it's how a live-fetched rate table
// (internal/provider's PricingCache, sourced from LiteLLM's
// model_prices_and_context_window.json) feeds into cost estimation without
// this package importing internal/provider, which already imports this one
// (cli_subprocess.go) and would make that a cycle. It prices a whole
// request, prompt-cache reads and writes included, and returns ok=false for
// a model it has no rate for.
var livePricingSource func(model string, usage protocol.UsageData) (usd float64, ok bool)

// SetLivePricingSource wires a live rate lookup into EstimateCostUSD. Called
// once at startup (see cmd/yoakeland/main.go) with internal/provider's
// PricingCache.CostUSD; left unset (nil), EstimateCostUSD falls back to the
// static table below exactly as before this existed — every existing test
// and any caller that never calls this keeps working unchanged.
func SetLivePricingSource(source func(model string, usage protocol.UsageData) (float64, bool)) {
	livePricingSource = source
}

// pricingTable is the last-resort fallback once livePricingSource has
// nothing for a model either (unset entirely, a fetch that's never
// succeeded and the embedded snapshot doesn't have this exact model, or a
// model string too far off LiteLLM's naming for PricingCache.Lookup's
// candidates to match). Covers a handful of common models as of writing
// (September 2026); an unlisted model prices at zero rather than guessing.
// ponytail: static table, no live pricing fetch of its own — update entries
// as needed, though livePricingSource should catch most drift first.
var pricingTable = map[string]rate{
	"claude-3-5-sonnet-20241022":  {InputPerMTok: 3, OutputPerMTok: 15},
	"claude-3-5-haiku-20241022":   {InputPerMTok: 0.8, OutputPerMTok: 4},
	"claude-3-opus-20240229":      {InputPerMTok: 15, OutputPerMTok: 75},
	"claude-opus-5-5":             {InputPerMTok: 4, OutputPerMTok: 20},
	"claude-opus-5":               {InputPerMTok: 5, OutputPerMTok: 25},
	"claude-sonnet-5":             {InputPerMTok: 2, OutputPerMTok: 10},
	"claude-haiku-4-5":            {InputPerMTok: 1, OutputPerMTok: 5},
	"claude-haiku-4-5-20251001":   {InputPerMTok: 1, OutputPerMTok: 5},
	"gpt-4o":                      {InputPerMTok: 2.5, OutputPerMTok: 10},
	"gpt-4o-mini":                 {InputPerMTok: 0.15, OutputPerMTok: 0.6},
	"gpt-4-turbo":                 {InputPerMTok: 10, OutputPerMTok: 30},
	"anthropic/claude-3.5-sonnet": {InputPerMTok: 3, OutputPerMTok: 15},
	// DeepSeek's real pricing is tiered (off-peak/peak, cache hit/miss) in a
	// way this flat table can't represent; this is the off-peak cache-miss
	// input / output rate as a reasonable single approximation. The legacy
	// "deepseek-chat"/"deepseek-reasoner" names are retired and now route to
	// DeepSeek-V4.1-Flash, priced the same either way.
	"deepseek-chat":            {InputPerMTok: 0.15, OutputPerMTok: 0.6},
	"deepseek-reasoner":        {InputPerMTok: 0.15, OutputPerMTok: 0.6},
	"deepseek/deepseek-chat":   {InputPerMTok: 0.15, OutputPerMTok: 0.6},
	"meta-llama/llama-3.1-70b": {InputPerMTok: 0.4, OutputPerMTok: 0.4},
}

// EstimateCostUSD prices a turn's token usage for the given model: the live
// rate table first (see SetLivePricingSource), the static table if that has
// nothing, $0 if neither does. Cost is informational, not load-bearing, so
// an unpriced model never errors — it just reports zero.
func EstimateCostUSD(model string, usage protocol.UsageData) float64 {
	if livePricingSource != nil {
		if usd, ok := livePricingSource(model, usage); ok {
			return usd
		}
	}

	r, ok := pricingTable[model]
	if !ok {
		return 0
	}
	// ponytail: Anthropic's cache multipliers (0.1× reads, 1.25× writes)
	// for every model here; the live table carries each model's real rates.
	plain := usage.InputTokens - usage.CachedInputTokens - usage.CacheWriteInputTokens
	input := float64(plain) + 0.1*float64(usage.CachedInputTokens) + 1.25*float64(usage.CacheWriteInputTokens)
	return input/1_000_000*r.InputPerMTok + float64(usage.OutputTokens)/1_000_000*r.OutputPerMTok
}

// addUsage adds u's token counts (not its cost) into dst.
func addUsage(dst *protocol.UsageData, u protocol.UsageData) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.TotalTokens += u.TotalTokens
	dst.CachedInputTokens += u.CachedInputTokens
	dst.CacheWriteInputTokens += u.CacheWriteInputTokens
}
