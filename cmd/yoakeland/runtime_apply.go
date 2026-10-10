package main

import (
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sandbox"
)

// applyProcessLimits pushes the runtime settings that live in package-level
// state (sandbox caps, the CLI process pool, the pricing refresh interval,
// the compaction defaults)
// into the packages that read them. It is a ServerSettings.OnRuntimeChange
// hook: idempotent, run once at registration and after each change. Commands
// and terminals already running keep the caps they started with.
func applyProcessLimits(pricing *provider.PricingCache) func(protocol.RuntimeSettings) {
	return func(rs protocol.RuntimeSettings) {
		sandbox.SetLimits(sandbox.Limits{
			ExecTimeout:          time.Duration(rs.ExecTimeoutSec) * time.Second,
			ExecOutputMaxBytes:   rs.ExecOutputMaxBytes,
			TerminalMemoryBytes:  int64(rs.TerminalMemoryMaxMB) << 20,
			TerminalPids:         rs.TerminalPidsMax,
			TerminalReadMaxChars: rs.TerminalReadMaxChars,
			TerminalHistoryBytes: rs.TerminalHistoryKB << 10,
		})
		provider.SetPoolLimits(time.Duration(rs.PoolIdleMinutes)*time.Minute, rs.PoolMaxLive)
		agent.SetCompactDefaults(rs.AutoCompactTokens, rs.CompactInstructions)
		pricing.SetRefreshInterval(time.Duration(rs.PricingRefreshHours) * time.Hour)
	}
}
