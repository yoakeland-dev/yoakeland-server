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

// TestAgent_RunTurnWithNilProvider guards a server started with no default
// provider (e.g. no API key/env var configured, relying entirely on
// register_provider/set_provider over the wire): a turn on a session that
// never got a provider must be rejected cleanly, not panic on a nil
// interface method call.
func TestAgent_RunTurnWithNilProvider(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, nil, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)

	err := ag.RunTurn(context.Background(), "session-no-provider", "turn-1", "hello")
	if err == nil {
		t.Fatal("expected an error when no provider is configured")
	}
	if !strings.Contains(outBuf.String(), `"finishReason":"no_provider"`) {
		t.Fatalf("expected a turn_completed with finishReason no_provider, got: %s", outBuf.String())
	}
}

// TestAgent_SetProviderThenRunTurn guards the recovery path: after
// SetProvider is called (mirroring set_provider over the wire), the same
// session that had no provider must work normally.
func TestAgent_SetProviderThenRunTurn(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, nil, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)

	scripted := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "now it works"}}},
	}
	ag.SetProvider(scripted, "test-provider")

	if err := ag.RunTurn(context.Background(), "session-recovered", "turn-1", "hello"); err != nil {
		t.Fatalf("expected the turn to succeed once a provider is set: %v", err)
	}
	if !strings.Contains(outBuf.String(), `"finishReason":"stop"`) {
		t.Fatalf("expected a normal completion, got: %s", outBuf.String())
	}
}

// TestAgent_EditRetryWithNilProvider guards EditMessage/RetryMessage against
// the same no-provider case RunTurn already guards, via the shared
// beginTurn — a session with no provider configured must reject an edit or
// a retry cleanly too, not just a fresh prompt.
func TestAgent_EditRetryWithNilProvider(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	scripted := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "hi there"}}},
	}
	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, scripted, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "session-edit-no-provider", "turn-1", "hello"); err != nil {
		t.Fatalf("initial turn: %v", err)
	}
	state, err := sessStore.Load("session-edit-no-provider")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID := state.History[0].ID
	assistantID := state.History[1].ID

	// Simulate the provider having been unset since (e.g. remove_provider).
	ag.SetProvider(nil, "")
	outBuf.Reset()

	if err := ag.EditMessage(context.Background(), "session-edit-no-provider", "turn-2", userID, "hello again", false); err == nil {
		t.Fatal("expected EditMessage to be refused with no provider configured")
	}
	if !strings.Contains(outBuf.String(), `"finishReason":"no_provider"`) {
		t.Fatalf("expected a turn_completed with finishReason no_provider, got: %s", outBuf.String())
	}

	outBuf.Reset()
	if err := ag.RetryMessage(context.Background(), "session-edit-no-provider", "turn-3", assistantID); err == nil {
		t.Fatal("expected RetryMessage to be refused with no provider configured")
	}
	if !strings.Contains(outBuf.String(), `"finishReason":"no_provider"`) {
		t.Fatalf("expected a turn_completed with finishReason no_provider, got: %s", outBuf.String())
	}
}
