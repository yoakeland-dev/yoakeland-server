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
	"yoakeland/internal/tool"
)

// TestRegistry_SessionsAreIsolated guards the bug the registry exists to fix:
// before it, a single shared Agent's sessionID/history were overwritten
// whenever a different session's turn came in, corrupting concurrent
// conversations. Each session must get its own Agent with its own history.
func TestRegistry_SessionsAreIsolated(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "reply to A"}},
			{{Kind: agent.StreamChunkDelta, Delta: "reply to B"}},
		},
	}

	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	registry := agent.NewRegistry(provider, []tool.Tool{}, engine, snapStore, sessStore, ctxMgr, nil)

	agA := registry.Get(ipc, "session-A")
	agB := registry.Get(ipc, "session-B")

	if agA == agB {
		t.Fatal("expected distinct sessions to get distinct Agents")
	}

	ctx := context.Background()
	if err := agA.RunTurn(ctx, "session-A", "turn-1", "hello from A"); err != nil {
		t.Fatalf("session A turn failed: %v", err)
	}
	if err := agB.RunTurn(ctx, "session-B", "turn-1", "hello from B"); err != nil {
		t.Fatalf("session B turn failed: %v", err)
	}

	if agA.CurrentSessionID() != "session-A" {
		t.Fatalf("session A's agent was clobbered: sessionID = %q", agA.CurrentSessionID())
	}
	if agB.CurrentSessionID() != "session-B" {
		t.Fatalf("session B's agent was clobbered: sessionID = %q", agB.CurrentSessionID())
	}

	// Fetching the same session again must return the same Agent (with its
	// history intact), not a fresh one.
	again := registry.Get(ipc, "session-A")
	if again != agA {
		t.Fatal("expected Get to return the existing Agent for a known session")
	}
}

// TestRegistry_ContextWindowIsPerSession guards against a shared
// *ContextManager bug: the registry used to hand every session's Agent the
// exact same ContextManager pointer, so one session resizing its context
// window via set_model_options (SetModelOptions's contextWindow) silently
// resized every *other* concurrent session in the project too — a model
// picker choosing a small-context model in one chat would make every other
// chat's "context full" readout wrong.
func TestRegistry_ContextWindowIsPerSession(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100000, 20)

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "reply to B"}},
		},
	}

	ipcA := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	ipcBOut := &bytes.Buffer{}
	ipcB := protocol.NewIPCStream(bytes.NewBuffer(nil), ipcBOut)
	registry := agent.NewRegistry(provider, []tool.Tool{}, engine, snapStore, sessStore, ctxMgr, nil)

	agA := registry.Get(ipcA, "session-A")
	agB := registry.Get(ipcB, "session-B")

	// Session A picks a small-context model.
	agA.SetModelOptions(agent.ModelOptions{}, 8000)

	// Session B, untouched, must still report the project's original
	// (large) context window — not A's.
	if err := agB.RunTurn(context.Background(), "session-B", "turn-1", "hello from B"); err != nil {
		t.Fatalf("session B turn failed: %v", err)
	}

	if !strings.Contains(ipcBOut.String(), `"maxTokens":100000`) {
		t.Fatalf("session B's context window was clobbered by session A's set_model_options; turn_completed: %s", ipcBOut.String())
	}
}

// TestRegistry_ReconnectNotifiesDisplacedConnection guards the reconnect UX:
// when a second device attaches to a session already bound to another
// connection, that first connection must be told it lost the session
// instead of silently going dark.
func TestRegistry_ReconnectNotifiedDisplacedConnection(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	registry := agent.NewRegistry(&ScriptedProvider{}, []tool.Tool{}, engine, snapStore, sessStore, ctxMgr, nil)

	firstOut := &bytes.Buffer{}
	firstConn := protocol.NewIPCStream(bytes.NewBuffer(nil), firstOut)
	registry.Get(firstConn, "session-shared")

	secondOut := &bytes.Buffer{}
	secondConn := protocol.NewIPCStream(bytes.NewBuffer(nil), secondOut)
	registry.Get(secondConn, "session-shared")

	if !strings.Contains(firstOut.String(), `"type":"session_taken_over"`) {
		t.Fatalf("expected displaced connection to receive a session_taken_over event, got: %s", firstOut.String())
	}
	if strings.Contains(secondOut.String(), `"type":"session_taken_over"`) {
		t.Fatalf("the connection that took over the session should not receive its own notice, got: %s", secondOut.String())
	}
}
