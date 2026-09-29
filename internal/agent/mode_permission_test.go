package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// syncBuf is a concurrency-safe io.Writer/String() pair — a test needs to
// poll the IPC output from a goroutine other than the one blocked inside
// RunTurn's permission-ask select, and a plain bytes.Buffer isn't safe for
// that concurrent read/write.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitForSubstring polls buf until it contains want or the timeout elapses.
func waitForSubstring(t *testing.T, buf *syncBuf, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output: %s", want, buf.String())
}

// TestAgent_ReadWriteModeForcesBashToAskAndDoesNotRemember guards tier 2's
// bash semantics: bash stays exposed, but every call is forced to Ask —
// bypassing the permission engine's safe-command auto-allowlist (e.g. a
// bare "pwd", which permission.Engine would otherwise auto-allow) — and a
// "remember" answer on that forced ask is never persisted, so the identical
// command asks again rather than silently auto-allowing itself later.
func TestAgent_ReadWriteModeForcesBashToAskAndDoesNotRemember(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)

	tools := []tool.Tool{tool.NewBashTool(norm, sb)}

	bashArgs, _ := json.Marshal(map[string]string{"command": "pwd"})

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc-bash-1", Name: "bash", Args: bashArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "done 1"}},
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc-bash-2", Name: "bash", Args: bashArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "done 2"}},
		},
	}

	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)
	ag.SetMode(agent.ModeReadWrite)

	// First turn: "pwd" would auto-allow under the engine's normal static
	// policy (see permission.Engine.Evaluate) — tier 2 must still force an
	// Ask. Answer it with remember: true.
	var asksChanged atomic.Int32
	ag.SetHooks(agent.Hooks{AsksChanged: func() { asksChanged.Add(1) }})
	pendingWhileAsking := make(chan []protocol.PendingAsk, 1)
	go func() {
		waitForSubstring(t, out, `"toolCallId":"tc-bash-1"`, 2*time.Second)
		pendingWhileAsking <- ag.PendingAsks()
		ag.HandlePermissionResponse("tc-bash-1", true, true, "")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t1", "run pwd"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// The ask is listed while open (for pending_asks on reconnect), and gone
	// once answered, with a change signalled each way.
	if p := <-pendingWhileAsking; len(p) != 1 || p[0].Kind != "permission" || p[0].ToolCallID != "tc-bash-1" || p[0].PermissionRequestData == nil {
		t.Fatalf("pending asks while asking = %+v", p)
	}
	if p := ag.PendingAsks(); len(p) != 0 || asksChanged.Load() != 2 {
		t.Fatalf("after answer: pending %+v, %d change signals", p, asksChanged.Load())
	}
	if !strings.Contains(out.String(), `"toolCallId":"tc-bash-1"`) || !strings.Contains(out.String(), "permission_request") {
		t.Fatalf("expected a permission_request for the forced-ask bash call, got: %s", out.String())
	}

	// Second turn, identical command: if the "remember" answer above had
	// been persisted, this would auto-allow with no second ask. It must ask
	// again.
	go func() {
		waitForSubstring(t, out, `"toolCallId":"tc-bash-2"`, 2*time.Second)
		ag.HandlePermissionResponse("tc-bash-2", true, false, "")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t2", "run pwd again"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(out.String(), `"toolCallId":"tc-bash-2"`) {
		t.Fatalf("expected a second permission_request — remembering a forced-ask decision must not persist, got: %s", out.String())
	}
}

// TestAgent_AutonomousModeNeverAsks guards tier 3: no permission_request
// fires at all, even for a tool the engine would normally Ask about.
func TestAgent_AutonomousModeNeverAsks(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{tool.NewFileWriteTool(norm)}
	writeArgs, _ := json.Marshal(map[string]string{"path": "auto.txt", "content": "x"})

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc1", Name: "file_write", Args: writeArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "done"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)
	ag.SetMode(agent.ModeAutonomous)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "write it"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if strings.Contains(outBuf.String(), "permission_request") {
		t.Fatalf("expected no permission_request in autonomous mode, got: %s", outBuf.String())
	}
}

// TestAgent_WebToolsToggleFiltersToolDefs guards Feature 2's composition
// with mode-based filtering: disabling web tools removes them from the
// model-visible tool defs regardless of mode, and re-enabling restores them.
func TestAgent_WebToolsToggleFiltersToolDefs(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewWebSearchTool(""),
		tool.NewWebFetchTool(),
	}

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "enabled turn"}},
			{{Kind: agent.StreamChunkDelta, Delta: "disabled turn"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "hi"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if got := provider.toolNamesByTurn[0]; len(got) != 4 {
		t.Fatalf("expected all 3 tools + subagent with web tools enabled (default), got %v", got)
	}

	ag.SetWebToolsEnabled(false)
	if ag.WebToolsEnabled() {
		t.Fatal("expected WebToolsEnabled() to report false")
	}
	if err := ag.RunTurn(context.Background(), "s1", "t2", "hi again"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	got := provider.toolNamesByTurn[1]
	if len(got) != 2 || got[0] != "file_read" || got[1] != agent.SubagentToolName {
		t.Fatalf("expected only file_read + subagent with web tools disabled, got %v", got)
	}
}

// TestAgent_AskUserQuestionBlocksAndResumes guards ask_user_question's
// always-blocking round trip: the turn pauses on ask_user_question_requested
// and resumes only once HandleAskUserAnswer resolves it, regardless of mode
// (it's not a mutation, so it isn't gated by permission.Engine at all).
func TestAgent_AskUserQuestionBlocksAndResumes(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{tool.NewAskUserQuestionTool()}
	askArgs, _ := json.Marshal(map[string]any{"question": "which approach?", "options": []string{"a", "b"}})

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc-ask-1", Name: "ask_user_question", Args: askArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "thanks"}},
		},
	}

	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)
	// Even in ModeReadOnly, ask_user_question must stay available.
	ag.SetMode(agent.ModeReadOnly)

	go func() {
		waitForSubstring(t, out, "ask_user_question_requested", 2*time.Second)
		ag.HandleAskUserAnswer("tc-ask-1", "b")
	}()

	if err := ag.RunTurn(context.Background(), "s1", "t1", "ask me"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(out.String(), "which approach?") {
		t.Fatalf("expected the question in ask_user_question_requested, got: %s", out.String())
	}
}

// A remembered decision is keyed on the field the tool acts on, so a stray
// field can't borrow another target's remembered allow.
func TestPermissionTarget_UsesToolsOwnField(t *testing.T) {
	args := json.RawMessage(`{"command":"notes.md","path":"./.git/config","content":"x"}`)
	if got := agent.PermissionTarget("file_write", args); got != ".git/config" {
		t.Fatalf("file_write target = %q", got)
	}
	if got := agent.PermissionTarget("bash", json.RawMessage(`{"path":"a","command":"ls"}`)); got != "ls" {
		t.Fatalf("bash target = %q", got)
	}
	if got := agent.PermissionTarget("web_fetch", json.RawMessage(`{"url":"https://Docs.Example.com/a?k=secret"}`)); got != "docs.example.com" {
		t.Fatalf("web_fetch target = %q", got)
	}
	if got := agent.PermissionTarget("browser", json.RawMessage(`{"action":"navigate","url":"https://example.com/x"}`)); got != "example.com" {
		t.Fatalf("browser navigate target = %q", got)
	}
	if got := agent.PermissionTarget("new_tool", args); got != string(args) {
		t.Fatalf("unknown tool target = %q", got)
	}
}
