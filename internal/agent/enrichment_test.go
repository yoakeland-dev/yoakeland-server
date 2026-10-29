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
	if data.Context.MaxTokens != 8000 {
		t.Fatalf("context.maxTokens = %d, want 8000 (the configured budget 100, raised to the compaction minimum, as no window is known)", data.Context.MaxTokens)
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

// unpricedProvider runs on a model no rate table knows.
type unpricedProvider struct{ *ScriptedProvider }

func (unpricedProvider) ModelName() string { return "some-unlisted-local-model" }

// A turn on a model with no known rate is reported as unpriced (usage
// costUnknown, spend *CostUnknown) instead of quietly counting as free.
func TestAgent_TurnCompletedFlagsUnknownCost(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	usageStore, err := agent.NewUsageStore(tempDir)
	if err != nil {
		t.Fatalf("NewUsageStore: %v", err)
	}
	provider := unpricedProvider{&ScriptedProvider{turns: [][]agent.ModelStreamChunk{{
		{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 500, OutputTokens: 50, TotalTokens: 550}},
		{Kind: agent.StreamChunkDelta, Delta: "done"},
	}}}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", []tool.Tool{}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100, 20), usageStore)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "session-unpriced", "turn-1", "Hello"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	data := lastTurnCompleted(t, outBuf.String())
	if data.Usage == nil || !data.Usage.CostUnknown || data.Usage.CostKnown || data.Usage.CostUSD != 0 {
		t.Fatalf("expected an unpriced turn, got %+v", data.Usage)
	}
	if data.Spend == nil || !data.Spend.SessionCostUnknown || !data.Spend.ProjectCostUnknown {
		t.Fatalf("expected the spend totals flagged unknown, got %+v", data.Spend)
	}
	if !usageStore.SessionTotal("session-unpriced").CostUnknown {
		t.Fatal("expected the unknown-cost flag to be persisted with the session's totals")
	}
	if data.Context == nil || data.Context.Source != "provider" || data.Context.EstimatedTokens != 550 {
		t.Fatalf("expected the provider's count as the context figure, got %+v", data.Context)
	}
}

// nativeScripted plays a CLI agent that runs its own tools.
type nativeScripted struct{ *ScriptedProvider }

func (nativeScripted) RunsOwnTools() {}

// A native agent's turn usage sums its requests; the context meter must use
// the last request's snapshot and the window the agent reported.
func TestAgent_TurnCompletedUsesNativeContextSnapshot(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	prov := nativeScripted{&ScriptedProvider{turns: [][]agent.ModelStreamChunk{{
		{Kind: agent.StreamChunkDelta, Delta: "done"},
		{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{
			InputTokens: 900_000, OutputTokens: 4000, TotalTokens: 904_000, CostUSD: 1, CostKnown: true,
			ContextTokens: 60_000, WindowTokens: 200_000,
		}},
	}}}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, prov, "native", []tool.Tool{}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "session-native", "turn-1", "Hello"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	c := lastTurnCompleted(t, outBuf.String()).Context
	if c == nil || c.EstimatedTokens != 60_000 || c.MaxTokens != 200_000 || c.WindowTokens != 200_000 || c.Source != "provider" {
		t.Fatalf("got %+v", c)
	}
}

// A provider's mid-turn context reading rides out to clients as turn_context,
// without touching the turn's own usage accounting.
func TestAgent_TurnContextEventDuringTurn(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{{
		{Kind: agent.StreamChunkContext, Context: &agent.ContextSnapshot{ContextTokens: 42_000, WindowTokens: 200_000, Source: "provider"}},
		{Kind: agent.StreamChunkDelta, Delta: "done"},
	}}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", []tool.Tool{}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "session-live", "turn-1", "Hello"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	var found *protocol.TurnContextData
	for _, line := range strings.Split(strings.TrimSpace(outBuf.String()), "\n") {
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil || env.Type != protocol.EventTurnContext {
			continue
		}
		var d protocol.TurnContextData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			t.Fatalf("malformed turn_context data: %v", err)
		}
		found = &d
	}
	if found == nil {
		t.Fatalf("no turn_context event found in output: %s", outBuf.String())
	}
	if found.EstimatedTokens != 42_000 || found.WindowTokens != 200_000 || found.Source != "provider" {
		t.Fatalf("turn_context = %+v", *found)
	}
}
