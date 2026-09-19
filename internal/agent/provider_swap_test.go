package agent_test

import (
	"bytes"
	"context"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
)

// TestAgent_SetProviderTakesEffectOnNextTurn guards hot model/provider
// switching: a session must keep using its original provider for a turn
// already in flight, then pick up the newly set one on the very next turn.
func TestAgent_SetProviderTakesEffectOnNextTurn(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	providerA := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "from A"}}},
	}
	providerB := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "from B"}}},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, providerA, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "session-swap", "turn-1", "hi"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if providerA.index != 1 {
		t.Fatalf("expected turn 1 to consume providerA, providerA.index = %d", providerA.index)
	}

	ag.SetProvider(providerB, "test-provider-b")

	if err := ag.RunTurn(context.Background(), "session-swap", "turn-2", "hi again"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if providerB.index != 1 {
		t.Fatalf("expected turn 2 to consume providerB after SetProvider, providerB.index = %d", providerB.index)
	}
	if providerA.index != 1 {
		t.Fatalf("providerA should not have been used again after the swap, providerA.index = %d", providerA.index)
	}
}
