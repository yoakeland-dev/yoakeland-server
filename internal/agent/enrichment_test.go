package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// ModelName makes ScriptedProvider satisfy the optional interface Agent uses
// to price a turn's usage against the pricing table.
func (s *ScriptedProvider) ModelName() string { return "claude-3-5-sonnet-20241022" }

// lastTurnCompleted scans NDJSON output for the last turn_completed envelope.
func lastTurnCompleted(t *testing.T, raw string) protocol.TurnCompletedData {
	t.Helper()
	var found protocol.TurnCompletedData
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if env.Type != protocol.EventTurnCompleted {
			continue
		}
		if err := json.Unmarshal(env.Data, &found); err != nil {
			t.Fatalf("malformed turn_completed data: %v", err)
		}
		seen = true
	}
	if !seen {
		t.Fatalf("no turn_completed event found in output: %s", raw)
	}
	return found
}

// TestAgent_TurnCompletedReportsContextAndSpend guards the spend-tracking and
// context-meter wiring added to RunTurn: cost must be priced from the
// model's usage, running totals must come from the UsageStore, and context
// fullness must reflect the configured token budget.
func TestAgent_TurnCompletedReportsContextAndSpend(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)
	usageStore, err := agent.NewUsageStore(tempDir)
	if err != nil {
		t.Fatalf("NewUsageStore: %v", err)
	}

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000}},
				{Kind: agent.StreamChunkDelta, Delta: "done"},
			},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", []tool.Tool{}, engine, snapStore, sessStore, ctxMgr, usageStore)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "session-cost", "turn-1", "Hello"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	data := lastTurnCompleted(t, outBuf.String())

	if data.Usage == nil {
		t.Fatal("expected usage data on turn_completed")
	}
	wantCost := 3.0 + 15.0 // $3/M input + $15/M output at 1M tokens each, per the pricing table
	if data.Usage.CostUSD != wantCost {
		t.Fatalf("turn cost = %v, want %v", data.Usage.CostUSD, wantCost)
	}

	if data.Context == nil {
		t.Fatal("expected context data on turn_completed")
	}
	if data.Context.MaxTokens != 100 {
		t.Fatalf("context.maxTokens = %d, want 100 (the configured budget)", data.Context.MaxTokens)
	}
	if data.Context.EstimatedTokens <= 0 {
		t.Fatalf("context.estimatedTokens = %d, want > 0 after a turn with history", data.Context.EstimatedTokens)
	}

	if data.Spend == nil {
		t.Fatal("expected spend data on turn_completed since a UsageStore was provided")
	}
	if data.Spend.SessionTotalUSD != wantCost || data.Spend.ProjectTotalUSD != wantCost {
		t.Fatalf("spend totals = %+v, want session and project both %v", data.Spend, wantCost)
	}
}
