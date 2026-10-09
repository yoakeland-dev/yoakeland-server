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
// (September 2026); an unlisted model is reported unpriced rather than guessed.
// The table has no live fetch of its own; livePricingSource catches most
// drift first, so entries only need updating for models it can't match.
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

// EstimateCost prices a turn's token usage for the given model: the live
// rate table first (see SetLivePricingSource), the static table if that has
// nothing. known is false when neither has a rate for model, in which case
// usd is 0 and means "unpriced", not "free": callers flag it (CostUnknown)
// instead of letting it read as a genuine $0.
func EstimateCost(model string, usage protocol.UsageData) (usd float64, known bool) {
	if livePricingSource != nil {
		if usd, ok := livePricingSource(model, usage); ok {
			return usd, true
		}
	}

	r, ok := pricingTable[model]
	if !ok {
		return 0, false
	}
	// Anthropic's cache multipliers (0.1x reads, 1.25x writes) for every
	// model here, wrong for the non-Anthropic entries; the live table
	// carries each model's real rates, so add per-entry multipliers only if
	// those models fall through to this table in practice.
	plain := usage.InputTokens - usage.CachedInputTokens - usage.CacheWriteInputTokens
	input := float64(plain) + 0.1*float64(usage.CachedInputTokens) + 1.25*float64(usage.CacheWriteInputTokens)
	return input/1_000_000*r.InputPerMTok + float64(usage.OutputTokens)/1_000_000*r.OutputPerMTok, true
}

// EstimateCostUSD is EstimateCost without the known flag: an unpriced model
// reads as $0.
func EstimateCostUSD(model string, usage protocol.UsageData) float64 {
	usd, _ := EstimateCost(model, usage)
	return usd
}

// priceUsage settles u's cost unless it already is (a provider-reported
// cost, or a price set earlier): CostUSD from the rate tables, or
// CostUnknown when model has none. Usage with no tokens costs nothing
// whatever the model.
func priceUsage(model string, u *protocol.UsageData) {
	if u.CostKnown || u.CostUnknown || u.InputTokens+u.OutputTokens == 0 {
		return
	}
	if usd, ok := EstimateCost(model, *u); ok {
		u.CostUSD, u.CostKnown = usd, true
		return
	}
	u.CostUnknown = true
}

// addUsage adds u's token counts (not its cost) into dst. An unpriced part
// makes the sum unpriced too; a request's context size and window are
// snapshots, not sums, so the latest reported one wins.
func addUsage(dst *protocol.UsageData, u protocol.UsageData) {
	dst.CostUnknown = dst.CostUnknown || u.CostUnknown
	if u.ContextTokens > 0 {
		dst.ContextTokens = u.ContextTokens
	}
	if u.WindowTokens > 0 {
		dst.WindowTokens = u.WindowTokens
	}
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.TotalTokens += u.TotalTokens
	dst.CachedInputTokens += u.CachedInputTokens
	dst.CacheWriteInputTokens += u.CacheWriteInputTokens
}
