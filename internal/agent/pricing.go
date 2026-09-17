package agent

import "yoakeland/internal/protocol"

// rate is USD per 1M tokens.
type rate struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// pricingTable covers common models as of writing. Prices drift and new
// models appear regularly; an unlisted model prices at zero rather than
// guessing.
// ponytail: static table, no live pricing fetch — update entries as needed.
var pricingTable = map[string]rate{
	"claude-3-5-sonnet-20241022":  {InputPerMTok: 3, OutputPerMTok: 15},
	"claude-3-5-haiku-20241022":   {InputPerMTok: 0.8, OutputPerMTok: 4},
	"claude-3-opus-20240229":      {InputPerMTok: 15, OutputPerMTok: 75},
	"gpt-4o":                      {InputPerMTok: 2.5, OutputPerMTok: 10},
	"gpt-4o-mini":                 {InputPerMTok: 0.15, OutputPerMTok: 0.6},
	"gpt-4-turbo":                 {InputPerMTok: 10, OutputPerMTok: 30},
	"anthropic/claude-3.5-sonnet": {InputPerMTok: 3, OutputPerMTok: 15},
	"deepseek/deepseek-chat":      {InputPerMTok: 0.14, OutputPerMTok: 0.28},
	"meta-llama/llama-3.1-70b":    {InputPerMTok: 0.4, OutputPerMTok: 0.4},
}

// EstimateCostUSD prices a turn's token usage for the given model. Unknown
// models (local servers, new releases not yet in the table) return 0 rather
// than an error, since cost is informational, not load-bearing.
func EstimateCostUSD(model string, usage protocol.UsageData) float64 {
	r, ok := pricingTable[model]
	if !ok {
		return 0
	}
	return float64(usage.InputTokens)/1_000_000*r.InputPerMTok +
		float64(usage.OutputTokens)/1_000_000*r.OutputPerMTok
}
