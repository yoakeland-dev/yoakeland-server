package agent_test

import (
	"bytes"
	"context"
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
		t.Fatal("two RunTurn calls for the same session executed concurrently — turnMu did not serialize them")
	}
	if atomic.LoadInt32(&maxQueued) == 0 || ag.QueuedTurns() != 0 {
		t.Fatalf("queued turns: max seen %d, after %d — turns waiting on turnMu must be counted, then drain", maxQueued, ag.QueuedTurns())
	}

	got := usageStore.SessionTotal("session-concurrent").CostUSD
	want := 0.01 * numTurns
	if got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("session spend total = %v, want %v (a lost update means concurrent turns raced on the same read-modify-write)", got, want)
	}
}
