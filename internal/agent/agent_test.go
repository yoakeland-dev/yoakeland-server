package agent_test

import (
	"bytes"
	"context"
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

type ScriptedProvider struct {
	turns [][]agent.ModelStreamChunk
	index int
}

func (s *ScriptedProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 10)
	eChan := make(chan error, 1)

	if s.index < len(s.turns) {
		turn := s.turns[s.index]
		s.index++
		go func() {
			for _, item := range turn {
				cChan <- item
			}
			close(cChan)
			close(eChan)
		}()
	} else {
		close(cChan)
		close(eChan)
	}
	return cChan, eChan
}

// HistoryRecordingProvider wraps ScriptedProvider's turn-per-call script but
// also records the history it was handed on each call, so a test can assert
// what the *next* round-trip would actually send.
type HistoryRecordingProvider struct {
	ScriptedProvider
	seenHistory [][]agent.Message
}

func (h *HistoryRecordingProvider) StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	h.seenHistory = append(h.seenHistory, history)
	return h.ScriptedProvider.StreamTurn(ctx, history, tools)
}

type BlockingProvider struct {
	started chan struct{}
}

func (b *BlockingProvider) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 2)
	eChan := make(chan error, 1)

	close(b.started)
	cChan <- agent.ModelStreamChunk{
		Kind:  agent.StreamChunkUsage,
		Usage: &protocol.UsageData{InputTokens: 50, OutputTokens: 25, TotalTokens: 75},
	}
	cChan <- agent.ModelStreamChunk{
		Kind:  agent.StreamChunkDelta,
		Delta: "Beginning work...",
	}

	go func() {
		<-ctx.Done()
		close(cChan)
		close(eChan)
	}()

	return cChan, eChan
}

func TestAgent_MultiTurnSessionPersistenceAndContextManagement(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 2*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
	}

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{Kind: agent.StreamChunkDelta, Delta: "Turn 1 complete"},
			},
			{
				{Kind: agent.StreamChunkDelta, Delta: "Turn 2 complete"},
			},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, tools, engine, snapStore, sessStore, ctxMgr, nil)

	ctx := context.Background()

	if err := ag.RunTurn(ctx, "session-alpha", "turn-1", "Hello 1"); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	if err := ag.RunTurn(ctx, "session-alpha", "turn-2", "Hello 2"); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	savedSession, err := sessStore.Load("session-alpha")
	if err != nil {
		t.Fatalf("failed loading saved session: %v", err)
	}
	if len(savedSession.History) != 4 {
		t.Fatalf("expected 4 messages in history, got %d", len(savedSession.History))
	}
}

func TestAgent_TurnCancellationAndMetrics(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	provider := &BlockingProvider{started: make(chan struct{})}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, []tool.Tool{tool.NewBashTool(norm, sb)}, engine, snapStore, sessStore, ctxMgr, nil)

	go func() {
		<-provider.started
		ag.CancelTurn("turn-cancel-1")
	}()

	err := ag.RunTurn(context.Background(), "session-test", "turn-cancel-1", "Test cancellation")
	if err == nil {
		t.Fatalf("expected error from cancelled turn, got nil")
	}

	output := outBuf.String()
	if !bytes.Contains(outBuf.Bytes(), []byte(`"finishReason":"cancelled"`)) {
		t.Errorf("expected finishReason cancelled in completed event, got: %s", output)
	}
}

// TestAgent_ReportsProviderFinishReason guards that a provider's own
// StreamChunkFinish (e.g. Anthropic's stop_reason "max_tokens" / OpenAI's
// finish_reason "length", both normalized to "length" by the provider
// layer) reaches turn_completed instead of being flattened to the default
// "stop" that a tool-call-free round-trip otherwise always got — the bug
// that made a truncated answer look identical to a complete one.
func TestAgent_ReportsProviderFinishReason(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{
		{
			{Kind: agent.StreamChunkDelta, Delta: "this got cut off"},
			{Kind: agent.StreamChunkFinish, FinishReason: "length"},
		},
	}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, []tool.Tool{tool.NewBashTool(norm, sb)}, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "session-test", "turn-length-1", "Write me something long"); err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}

	output := outBuf.String()
	if !bytes.Contains(outBuf.Bytes(), []byte(`"finishReason":"length"`)) {
		t.Errorf("expected finishReason length in completed event, got: %s", output)
	}
}

// TestAgent_EmptyRoundTripNotAddedToHistory guards against a round-trip that
// produces neither visible text nor a tool call — e.g. a reasoning model
// that exhausts its token budget on hidden reasoning before writing any
// content, finishing with finish_reason "length" and an empty delta —
// turning into an assistant message with no content and no tool_calls.
// Every provider's wire format requires one or the other, so a history
// entry shaped like that makes the *next* request invalid, not just this
// one: the follow-up turn fails outright instead of just this round
// finishing early.
func TestAgent_EmptyRoundTripNotAddedToHistory(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(1000, 200)

	// Three empty+length round-trips in a row exhausts the auto-continue
	// retry budget (see maxEmptyLengthRetries), so the first RunTurn ends
	// with a "length" finish and nothing added to history; the fourth
	// scripted turn answers the second RunTurn normally.
	emptyLengthChunks := []agent.ModelStreamChunk{
		{Kind: agent.StreamChunkFinish, FinishReason: "length"},
	}
	provider := &HistoryRecordingProvider{ScriptedProvider: ScriptedProvider{turns: [][]agent.ModelStreamChunk{
		emptyLengthChunks,
		emptyLengthChunks,
		emptyLengthChunks,
		{
			{Kind: agent.StreamChunkDelta, Delta: "ok, retrying"},
		},
	}}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, []tool.Tool{tool.NewBashTool(norm, sb)}, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "session-empty", "turn-1", "hello"); err != nil {
		t.Fatalf("first RunTurn returned error: %v", err)
	}
	if err := ag.RunTurn(context.Background(), "session-empty", "turn-2", "try again"); err != nil {
		t.Fatalf("second RunTurn returned error: %v", err)
	}

	if len(provider.seenHistory) != 4 {
		t.Fatalf("expected 4 StreamTurn calls (3 retries + the second RunTurn), got %d", len(provider.seenHistory))
	}
	for _, m := range provider.seenHistory[3] {
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("history sent to the second round-trip contains an empty assistant message: %+v", provider.seenHistory[3])
		}
	}
}

// TestAgent_EmptyLengthCutoffAutoContinues guards the auto-continue retry
// itself: a round-trip with no text, no tool call, and finish_reason
// "length" must not be reported to the user as-is — it should be retried
// (a fresh generation, since nothing was added to history to resend) until
// either it produces something or the retry budget runs out.
func TestAgent_EmptyLengthCutoffAutoContinues(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(1000, 200)

	provider := &HistoryRecordingProvider{ScriptedProvider: ScriptedProvider{turns: [][]agent.ModelStreamChunk{
		{{Kind: agent.StreamChunkFinish, FinishReason: "length"}},
		{{Kind: agent.StreamChunkDelta, Delta: "here it is"}},
	}}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, []tool.Tool{tool.NewBashTool(norm, sb)}, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "session-retry", "turn-1", "hello"); err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}

	if len(provider.seenHistory) != 2 {
		t.Fatalf("expected the empty length cutoff to be retried once (2 StreamTurn calls), got %d", len(provider.seenHistory))
	}
	if !bytes.Contains(outBuf.Bytes(), []byte(`"finishReason":"stop"`)) {
		t.Errorf("expected the retried turn to finish normally, got: %s", outBuf.String())
	}
	if bytes.Contains(outBuf.Bytes(), []byte(`"finishReason":"length"`)) {
		t.Errorf("the empty cutoff should not have been reported as the turn's outcome, got: %s", outBuf.String())
	}
}
