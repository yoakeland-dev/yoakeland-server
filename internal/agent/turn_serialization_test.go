package agent_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// blockingCostProvider calls onStart synchronously (from the goroutine that
// would otherwise be racing another turn) before reporting a fixed,
// known-authoritative cost for the turn.
type blockingCostProvider struct {
	onStart func()
	costUSD float64
}

func (p *blockingCostProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 2)
	eChan := make(chan error, 1)
	go func() {
		if p.onStart != nil {
			p.onStart()
		}
		cChan <- agent.ModelStreamChunk{
			Kind: agent.StreamChunkUsage,
			Usage: &protocol.UsageData{
				InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
				CostUSD: p.costUSD, CostKnown: true,
			},
		}
		cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ok"}
		close(cChan)
		close(eChan)
	}()
	return cChan, eChan
}

// TestAgent_RunTurn_SerializesConcurrentTurnsForSameSession guards the TOCTOU
// fix: two user_prompts dispatched for the same session at (near) the same
// time must not both execute concurrently — otherwise they could both pass
// the pre-turn spend check before either turn's cost is recorded, or
// interleave a.history/session-file writes.
func TestAgent_RunTurn_SerializesConcurrentTurnsForSameSession(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(1_000_000, 20)
	usageStore, err := agent.NewUsageStore(tempDir)
	if err != nil {
		t.Fatalf("NewUsageStore: %v", err)
	}

	var inFlight int32
	var sawOverlap int32
	var maxQueued int32
	var ag *agent.Agent
	provider := &blockingCostProvider{
		onStart: func() {
			if atomic.AddInt32(&inFlight, 1) > 1 {
				atomic.StoreInt32(&sawOverlap, 1)
			}
			if q := int32(ag.QueuedTurns()); q > atomic.LoadInt32(&maxQueued) {
				atomic.StoreInt32(&maxQueued, q)
			}
			time.Sleep(20 * time.Millisecond) // widen the race window
			atomic.AddInt32(&inFlight, -1)
		},
		costUSD: 0.01,
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag = agent.NewAgent(ipc, provider, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, usageStore)
	t.Cleanup(ag.WaitBackground)

	const numTurns = 5
	var wg sync.WaitGroup
	for i := 0; i < numTurns; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			turnID := "turn-" + string(rune('a'+n))
			_ = ag.RunTurn(context.Background(), "session-concurrent", turnID, "prompt")
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt32(&sawOverlap) != 0 {
		t.Fatal("two RunTurn calls for the same session executed concurrently — turnSlot did not serialize them")
	}
	if atomic.LoadInt32(&maxQueued) == 0 || ag.QueuedTurns() != 0 {
		t.Fatalf("queued turns: max seen %d, after %d — turns waiting on turnSlot must be counted, then drain", maxQueued, ag.QueuedTurns())
	}

	got := usageStore.SessionTotal("session-concurrent").CostUSD
	want := 0.01 * numTurns
	if got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("session spend total = %v, want %v (a lost update means concurrent turns raced on the same read-modify-write)", got, want)
	}
}

// TestAgent_WithdrawTurn_QueuedTurnNeverRuns: a prompt sent while another
// turn runs waits its turn, and withdrawing it before then (the client's
// "undo send") ends it as cancelled without ever touching history.
func TestAgent_WithdrawTurn_QueuedTurnNeverRuns(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)

	release := make(chan struct{})
	provider := &blockingCostProvider{onStart: func() { <-release }}
	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", nil, engine, snapStore, sessStore, agent.NewContextManager(1_000_000, 20), nil)
	t.Cleanup(ag.WaitBackground)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = ag.RunTurn(context.Background(), "s", "turn-a", "first") }()
	for ag.ActiveTurnCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { defer wg.Done(); _ = ag.RunTurn(context.Background(), "s", "turn-b", "second") }()
	for ag.QueuedTurns() == 0 {
		time.Sleep(time.Millisecond)
	}

	if ag.WithdrawTurn("turn-a") {
		t.Fatal("WithdrawTurn withdrew a turn that had already started")
	}
	if !ag.WithdrawTurn("turn-b") {
		t.Fatal("WithdrawTurn didn't find the queued turn")
	}
	close(release)
	wg.Wait()

	st, err := sessStore.Load("s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, m := range st.History {
		if m.Content == "second" {
			t.Fatal("the withdrawn prompt reached history")
		}
	}
	out := outBuf.String()
	for _, want := range []string{
		`"type":"turn_started","sessionId":"s","turnId":"turn-a"`,
		`"type":"turn_completed","sessionId":"s","turnId":"turn-b","data":{"finishReason":"cancelled"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"type":"turn_started","sessionId":"s","turnId":"turn-b"`) {
		t.Fatalf("the withdrawn turn was announced as started:\n%s", out)
	}
}
