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

// waitForAsk polls until toolCallID is a pending ask, the point from which
// HandlePermissionResponse lands. Its id shows up in the output sooner, on
// tool_call_requested, so waiting on that text can answer too early and be dropped.
func waitForAsk(t *testing.T, ag *agent.Agent, toolCallID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range ag.PendingAsks() {
			if p.ToolCallID == toolCallID {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("timed out waiting for a pending ask on %q", toolCallID)
}

// TestAgent_ReadWriteModeAsksForBashUntilARuleAllowsIt: bash stays exposed and
// every call asks when no rule covers it (not even a "read-only" `pwd`), and
// a remembered answer stores a rule so the identical command no longer asks.
func TestAgent_ReadWriteModeAsksForBashUntilARuleAllowsIt(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)

	tools := []tool.Tool{tool.NewBashTool(norm, sb)}

	call := func(id, cmd string) []agent.ModelStreamChunk {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		return []agent.ModelStreamChunk{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: "bash", Args: args}}}
	}
	done := func(s string) []agent.ModelStreamChunk {
		return []agent.ModelStreamChunk{{Kind: agent.StreamChunkDelta, Delta: s}}
	}
	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			call("tc-bash-1", "pwd"), done("done 1"),
			call("tc-bash-2", "pwd"), done("done 2"),
			call("tc-bash-3", "ls"), done("done 3"),
		},
	}

	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)
	ag.SetMode(agent.ModeReadWrite)

	// First turn: nothing allows `pwd`, so it asks. Answer with remember.
	var asksChanged atomic.Int32
	ag.SetHooks(agent.Hooks{AsksChanged: func() { asksChanged.Add(1) }})
	pendingWhileAsking := make(chan []protocol.PendingAsk, 1)
	go func() {
		waitForAsk(t, ag, "tc-bash-1", 5*time.Second)
		pendingWhileAsking <- ag.PendingAsks()
		ag.HandlePermissionResponse("tc-bash-1", true, true, "")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t1", "run pwd"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// The ask is listed while open (for pending_asks on reconnect), carries
	// the rules it could remember, and is gone once answered.
	p := <-pendingWhileAsking
	if len(p) != 1 || p[0].Kind != "permission" || p[0].ToolCallID != "tc-bash-1" || p[0].PermissionRequestData == nil {
		t.Fatalf("pending asks while asking = %+v", p)
	}
	if sg := p[0].PermissionRequestData.Suggestions; len(sg) != 2 || sg[0].ID != "prefix" || sg[1].ID != "exact" {
		t.Fatalf("suggestions = %+v", sg)
	}
	if p := ag.PendingAsks(); len(p) != 0 || asksChanged.Load() != 2 {
		t.Fatalf("after answer: pending %+v, %d change signals", p, asksChanged.Load())
	}
	if n := strings.Count(out.String(), `"type":"permission_request"`); n != 1 {
		t.Fatalf("expected one permission_request, got %d: %s", n, out.String())
	}

	// Second turn, identical command: the remembered rule allows it, no ask.
	if err := ag.RunTurn(context.Background(), "s1", "t2", "run pwd again"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if n := strings.Count(out.String(), `"type":"permission_request"`); n != 1 {
		t.Fatalf("a remembered command asked again (%d requests): %s", n, out.String())
	}
	if !strings.Contains(out.String(), `"toolCallId":"tc-bash-2"`) {
		t.Fatalf("second call never ran: %s", out.String())
	}

	// A different command still asks.
	go func() {
		waitForAsk(t, ag, "tc-bash-3", 5*time.Second)
		ag.HandlePermissionResponse("tc-bash-3", false, false, "")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t3", "run ls"); err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if n := strings.Count(out.String(), `"type":"permission_request"`); n != 2 {
		t.Fatalf("expected a second permission_request for ls, got %d", n)
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

// Several questions in one call are all pending together, each answerable on its
// own, and the turn resumes once the last is answered.
func TestAgent_AskUserQuestionsStackAndResume(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	askArgs, _ := json.Marshal(map[string]any{"questions": []map[string]any{{"question": "first?"}, {"question": "second?", "options": []string{"a", "b"}}}})
	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc", Name: "ask_user_question", Args: askArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "thanks"}},
		},
	}
	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, provider, "test-provider", []tool.Tool{tool.NewAskUserQuestionTool()}, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)

	go func() {
		waitForSubstring(t, out, "second?", 2*time.Second)
		if n := len(ag.PendingAsks()); n != 2 {
			t.Errorf("pending asks = %d, want 2", n)
		}
		ag.HandleAskUserAnswer("tc#1", "b")
		ag.HandleAskUserAnswer("tc#0", "yes")
	}()
	if err := ag.RunTurn(context.Background(), "s1", "t1", "ask me"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(out.String(), `"total":2`) {
		t.Fatalf("expected index/total on the questions, got: %s", out.String())
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
