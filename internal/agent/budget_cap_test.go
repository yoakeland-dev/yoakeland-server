package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
)

// TestAgent_MaxSessionSpendUSD guards the runaway-cost gate: once a session
// has already spent at or above its configured cap, RunTurn must refuse to
// even start the next turn rather than letting a loop of turns keep
// compounding past the limit.
func TestAgent_MaxSessionSpendUSD(t *testing.T) {
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

	providerA := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{
			{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
			{Kind: agent.StreamChunkDelta, Delta: "expensive turn"},
		}},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, providerA, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, usageStore)
	t.Cleanup(ag.WaitBackground)
	ag.SetMaxSessionSpendUSD(1.0) // claude-3-5-sonnet-20241022 pricing puts this turn at $18

	if err := ag.RunTurn(context.Background(), "session-budget", "turn-1", "do something expensive"); err != nil {
		t.Fatalf("first turn should be allowed to run (cap only gates the *next* turn): %v", err)
	}

	err = ag.RunTurn(context.Background(), "session-budget", "turn-2", "do it again")
	if err == nil {
		t.Fatal("expected the second turn to be refused once the session is over its spend cap")
	}

	if !strings.Contains(outBuf.String(), `"finishReason":"budget_exceeded"`) {
		t.Fatalf("expected a turn_completed with finishReason budget_exceeded, got: %s", outBuf.String())
	}
}

// TestAgent_MaxSessionSpendUSD_Unlimited guards the default: a cap of 0 must
// never block a turn, no matter how much a session has already spent.
func TestAgent_MaxSessionSpendUSD_Unlimited(t *testing.T) {
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
	usageStore.Record("session-unlimited", 0, 0, 999.0) // simulate a huge prior spend

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "fine"}}},
	}

	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	ag := agent.NewAgent(ipc, provider, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, usageStore)
	t.Cleanup(ag.WaitBackground)
	// No SetMaxSessionSpendUSD call: default must be unlimited.

	if err := ag.RunTurn(context.Background(), "session-unlimited", "turn-1", "hi"); err != nil {
		t.Fatalf("expected an unconfigured cap to never block a turn: %v", err)
	}
}

// TestAgent_MaxSessionSpendUSD_AppliesToEditAndRetry guards that
// EditMessage/RetryMessage go through the same beginTurn budget check
// RunTurn does — an edit or a retry is just as billed as an ordinary turn,
// so the cap must refuse it identically once a session is already over.
func TestAgent_MaxSessionSpendUSD_AppliesToEditAndRetry(t *testing.T) {
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
		turns: [][]agent.ModelStreamChunk{{
			{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
			{Kind: agent.StreamChunkDelta, Delta: "expensive turn"},
		}},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, usageStore)
	t.Cleanup(ag.WaitBackground)
	ag.SetMaxSessionSpendUSD(1.0)

	if err := ag.RunTurn(context.Background(), "session-budget-2", "turn-1", "do something expensive"); err != nil {
		t.Fatalf("first turn should be allowed to run: %v", err)
	}

	state, err := sessStore.Load("session-budget-2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID := state.History[0].ID
	assistantID := state.History[1].ID

	if err := ag.EditMessage(context.Background(), "session-budget-2", "turn-2", userID, "do it differently", false); err == nil {
		t.Fatal("expected EditMessage to be refused once the session is over its spend cap")
	}
	if err := ag.RetryMessage(context.Background(), "session-budget-2", "turn-3", assistantID); err == nil {
		t.Fatal("expected RetryMessage to be refused once the session is over its spend cap")
	}
}
