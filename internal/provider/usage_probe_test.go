package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"yoakeland/internal/protocol"
)

// Trimmed from a real `get_usage` answer (Claude Code 2.1.280, Pro plan),
// codenamed buckets included: they must be ignored, not break parsing.
const claudeUsageFixture = `{"session":{"total_cost_usd":0},"subscription_type":"pro","rate_limits_available":true,
"rate_limits":{"five_hour":{"utilization":22,"resets_at":"2026-09-24T05:19:59.596686+00:00","limit_dollars":null},
"seven_day":{"utilization":30,"resets_at":"2026-09-30T11:59:59.596706+00:00"},"seven_day_opus":null,"seven_day_sonnet":null,
"iguana_necktie":{"utilization":0,"resets_at":"2026-11-05T07:59:00+00:00","limit_dollars":100},
"model_scoped":[{"display_name":"Fable","utilization":12.5,"resets_at":"2026-09-30T11:59:59Z"}],
"extra_usage":{"is_enabled":false}},"behaviors":null}`

// From a real `account/rateLimits/read` (codex-cli 0.155.1, free plan),
// plus a second per-model bucket as a Plus plan reports one.
const codexUsageFixture = `{"ordinaryUsageAllowed":false,
"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":100,"windowDurationMins":43200,"resetsAt":1790790203},"secondary":null,"planType":"free"},
"rateLimitsByLimitId":{
 "codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":100,"windowDurationMins":43200,"resetsAt":1790790203},"secondary":null,"planType":"free"},
 "codex_spark":{"limitId":"codex_spark","limitName":"GPT-5.3-Codex-Spark","primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1790790203},"secondary":null}}}`

func TestProbeClaudeUsage(t *testing.T) {
	var args []string
	start, wait := fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		for in.Scan() {
			var req struct {
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
			}
			_ = json.Unmarshal(in.Bytes(), &req)
			resp := json.RawMessage(`{}`)
			if req.Request.Subtype == "get_usage" {
				resp = json.RawMessage(claudeUsageFixture)
			}
			line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": req.RequestID, "response": resp}})
		}
	})
	rl, err := probeRateLimits(context.Background(), CLIKindClaude, start)
	if err != nil {
		t.Fatal(err)
	}
	wait()
	if !slices.Contains(args, "--no-session-persistence") || !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("probe should run a throwaway session, got args %v", args)
	}
	if rl.Plan != "pro" || rl.FiveHour == nil || rl.FiveHour.UsedPct != 22 || rl.SevenDay == nil || rl.SevenDay.UsedPct != 30 {
		t.Fatalf("got %+v", rl)
	}
	if len(rl.Other) != 1 || rl.Other[0].Label != "Fable" || rl.Other[0].UsedPct != 12.5 || rl.Other[0].WindowMins != 7*24*60 {
		t.Fatalf("model-scoped window: got %+v", rl.Other)
	}
}

func TestProbeCodexUsage(t *testing.T) {
	var args []string
	start, wait := fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{}
			case "account/rateLimits/read":
				return json.RawMessage(codexUsageFixture)
			}
			return nil
		})
	})
	rl, err := probeRateLimits(context.Background(), CLIKindCodex, start)
	if err != nil {
		t.Fatal(err)
	}
	wait()
	if rl.Plan != "free" || rl.FiveHour != nil || rl.SevenDay != nil {
		t.Fatalf("got %+v", rl)
	}
	want := []protocol.OtherRateLimitWindow{
		{WindowMins: 43200, RateLimitWindow: protocol.RateLimitWindow{UsedPct: 100, ResetsAt: "2026-09-30T17:43:23Z"}},
		{WindowMins: 300, Label: "GPT-5.3-Codex-Spark", RateLimitWindow: protocol.RateLimitWindow{UsedPct: 40, ResetsAt: "2026-09-30T17:43:23Z"}},
	}
	if !slices.Equal(rl.Other, want) {
		t.Fatalf("got %+v, want %+v", rl.Other, want)
	}
}

func TestProbeUnsupportedCLI(t *testing.T) {
	if _, err := probeRateLimits(context.Background(), CLIKindGemini, nil); !errors.Is(err, ErrUsageUnsupported) {
		t.Fatalf("got %v", err)
	}
}

// A streamed rate_limit_event carries one window; merging it must keep the
// rest of what the last probe read.
func TestStreamedRateLimitMergesOntoProbe(t *testing.T) {
	probed, err := parseClaudeUsage(json.RawMessage(claudeUsageFixture))
	if err != nil {
		t.Fatal(err)
	}
	ev := parseClaudeRateLimits(json.RawMessage(`{"status":"allowed","rateLimitType":"five_hour","utilization":0.4,"resetsAt":1790790203}`))
	got := probed.Merge(*ev)
	if got.FiveHour.UsedPct != 40 || got.SevenDay == nil || got.SevenDay.UsedPct != 30 || len(got.Other) != 1 || got.Plan != "pro" {
		t.Fatalf("got %+v", got)
	}
}
