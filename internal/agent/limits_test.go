package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// noopTool needs no permission and does nothing, so a turn of them is a
// pure sequence of model round-trips.
type noopTool struct{ name string }

func (n noopTool) Name() string                    { return n.name }
func (n noopTool) Description() string             { return "" }
func (n noopTool) Definition() tool.ToolDefinition { return tool.ToolDefinition{Name: n.name} }
func (n noopTool) RequiresPermission([]byte) bool  { return false }
func (n noopTool) Execute(context.Context, tool.ExecutionContext, []byte) (string, error) {
	return "ok", nil
}

// loopProvider calls noop on every round until finishAfter rounds have been
// made (0: forever), then answers with text. Every round reports costPerRound.
type loopProvider struct {
	mu           sync.Mutex
	calls        int
	finishAfter  int
	costPerRound float64
	unpriced     bool // report tokens but no cost, on a model with no known rate
	toolNames    [][]string
}

func (p *loopProvider) StreamTurn(_ context.Context, _ []agent.Message, tools []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	var names []string
	for _, td := range tools {
		names = append(names, td.Name)
	}
	p.toolNames = append(p.toolNames, names)
	p.mu.Unlock()

	c, e := make(chan agent.ModelStreamChunk, 4), make(chan error, 1)
	if p.unpriced {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 5000, OutputTokens: 5000}}
	}
	if p.costPerRound > 0 {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 1, CostUSD: p.costPerRound, CostKnown: true}}
	}
	if p.finishAfter > 0 && n > p.finishAfter {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "done"}
	} else {
		args, _ := json.Marshal(map[string]int{"n": n})
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "call-" + string(rune('a'+n)), Name: "noop", Args: args}}
	}
	close(c)
	close(e)
	return c, e
}

func (p *loopProvider) roundsMade() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func limitAgent(t *testing.T, p agent.ModelProvider, withUsage bool, tools ...tool.Tool) (*agent.Agent, *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	var usage *agent.UsageStore
	if withUsage {
		var err error
		if usage, err = agent.NewUsageStore(dir); err != nil {
			t.Fatalf("NewUsageStore: %v", err)
		}
	}
	out := &syncBuf{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), p, "test-provider",
		append([]tool.Tool{noopTool{"noop"}}, tools...), permission.New(norm, nil), snapStore, sessStore,
		agent.NewContextManager(100000, 2000), usage)
	t.Cleanup(ag.WaitBackground)
	return ag, out
}

type turnResult struct{ err error }

func runTurnAsync(ag *agent.Agent, ctx context.Context, turnID string) <-chan turnResult {
	done := make(chan turnResult, 1)
	go func() { done <- turnResult{ag.RunTurn(ctx, "s1", turnID, "go")} }()
	return done
}

func waitTurn(t *testing.T, done <-chan turnResult) turnResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not finish")
		return turnResult{}
	}
}

// After N model round-trips the turn pauses on a question that surfaces as a
// pending ask; Continue runs N more, Stop ends the turn as limit_reached.
func TestLimits_RoundLimitPausesAndAsks(t *testing.T) {
	p := &loopProvider{}
	ag, out := limitAgent(t, p, false)
	ag.SetLimits(agent.NewLimits(2, 0))

	done := runTurnAsync(ag, context.Background(), "t1")
	waitForAsk(t, ag, "limit-t1-1", 5*time.Second)
	asks := ag.PendingAsks()
	if len(asks) != 1 || asks[0].Kind != "question" || asks[0].AskUserQuestionRequestedData == nil ||
		!strings.Contains(asks[0].Question, "2 model calls") || len(asks[0].Options) != 2 {
		t.Fatalf("pending asks = %+v", asks)
	}
	if n := p.roundsMade(); n != 2 {
		t.Fatalf("model called %d times before the pause, want 2", n)
	}

	ag.HandleAskUserAnswer("limit-t1-1", "Continue")
	waitForAsk(t, ag, "limit-t1-2", 5*time.Second)
	if n := p.roundsMade(); n != 4 {
		t.Fatalf("after Continue the model was called %d times, want 4 (another 2 rounds)", n)
	}

	ag.HandleAskUserAnswer("limit-t1-2", "Stop")
	if r := waitTurn(t, done); r.err != nil {
		t.Fatalf("stopping at the limit is not an error: %v", r.err)
	}
	if n := p.roundsMade(); n != 4 {
		t.Fatalf("model called %d times after Stop, want 4", n)
	}
	if !strings.Contains(out.String(), `"finishReason":"limit_reached"`) {
		t.Fatalf("expected finishReason limit_reached, got: %s", out.String())
	}
	if len(ag.PendingAsks()) != 0 {
		t.Fatalf("asks left behind: %+v", ag.PendingAsks())
	}
}

// Continue raises the ceiling for that turn only: the next turn starts over.
func TestLimits_ContinueIsPerTurn(t *testing.T) {
	p := &loopProvider{finishAfter: 3}
	ag, _ := limitAgent(t, p, false)
	ag.SetLimits(agent.NewLimits(2, 0))

	done := runTurnAsync(ag, context.Background(), "t1")
	waitForAsk(t, ag, "limit-t1-1", 5*time.Second)
	ag.HandleAskUserAnswer("limit-t1-1", "continue")
	if r := waitTurn(t, done); r.err != nil {
		t.Fatal(r.err)
	}

	p.mu.Lock()
	p.calls, p.finishAfter = 0, 0
	p.mu.Unlock()
	done = runTurnAsync(ag, context.Background(), "t2")
	waitForAsk(t, ag, "limit-t2-1", 5*time.Second) // asks again after 2 rounds, not after the raised ceiling
	ag.HandleAskUserAnswer("limit-t2-1", "Stop")
	waitTurn(t, done)
}

// Cancelling a paused turn drops its question and ends it as cancelled.
func TestLimits_CancelWhilePaused(t *testing.T) {
	p := &loopProvider{}
	ag, out := limitAgent(t, p, false)
	ag.SetLimits(agent.NewLimits(1, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := runTurnAsync(ag, ctx, "t1")
	waitForAsk(t, ag, "limit-t1-1", 5*time.Second)
	cancel()
	waitTurn(t, done)
	if !strings.Contains(out.String(), `"finishReason":"cancelled"`) {
		t.Fatalf("expected a cancelled turn, got: %s", out.String())
	}
	if len(ag.PendingAsks()) != 0 {
		t.Fatalf("the question outlived its turn: %+v", ag.PendingAsks())
	}
}

// 0 is unlimited, and a limit changed while a session exists reaches it.
func TestLimits_UnlimitedAndLive(t *testing.T) {
	p := &loopProvider{finishAfter: 6}
	ag, _ := limitAgent(t, p, false)
	shared := agent.NewLimits(0, 0)
	ag.SetLimits(shared)

	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t1")); r.err != nil {
		t.Fatal(r.err)
	}
	if len(ag.PendingAsks()) != 0 || p.roundsMade() != 7 {
		t.Fatalf("unlimited turn paused or stopped early: %d rounds", p.roundsMade())
	}

	p.mu.Lock()
	p.calls = 0
	p.mu.Unlock()
	shared.Set(1, 0) // an already-existing session sees the new limit on its next turn
	done := runTurnAsync(ag, context.Background(), "t2")
	waitForAsk(t, ag, "limit-t2-1", 5*time.Second)
	ag.HandleAskUserAnswer("limit-t2-1", "Stop")
	waitTurn(t, done)
}

// The cost cap is checked before every round of a running turn, not only at
// its start; reaching it pauses instead of killing, and Continue lets the turn
// spend one more cap's worth.
func TestLimits_CostPausesMidTurn(t *testing.T) {
	p := &loopProvider{costPerRound: 0.6}
	ag, out := limitAgent(t, p, true)
	ag.SetLimits(agent.NewLimits(0, 1.0))

	done := runTurnAsync(ag, context.Background(), "t1")
	waitForAsk(t, ag, "limit-t1-1", 5*time.Second)
	if n := p.roundsMade(); n != 2 { // $0.60, then $1.20 >= $1.00
		t.Fatalf("model called %d times before the cost pause, want 2", n)
	}
	if q := ag.PendingAsks()[0].Question; !strings.Contains(q, "$1.20") || !strings.Contains(q, "$1.00") {
		t.Fatalf("question = %q", q)
	}
	ag.HandleAskUserAnswer("limit-t1-1", "Continue")
	waitForAsk(t, ag, "limit-t1-2", 5*time.Second) // $1.80, $2.40 >= $2.00
	if n := p.roundsMade(); n != 4 {
		t.Fatalf("after Continue the model was called %d times, want 4", n)
	}
	ag.HandleAskUserAnswer("limit-t1-2", "Stop")
	if r := waitTurn(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(out.String(), `"finishReason":"limit_reached"`) {
		t.Fatalf("got: %s", out.String())
	}
}

// Unpriced usage counts as $0, as everywhere else: it can never trip the cap.
func TestLimits_UnpricedUsageDoesNotTripTheCap(t *testing.T) {
	p := &loopProvider{finishAfter: 3, unpriced: true}
	ag, _ := limitAgent(t, p, true)
	ag.SetLimits(agent.NewLimits(0, 0.01))
	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t1")); r.err != nil {
		t.Fatal(r.err)
	}
	if len(ag.PendingAsks()) != 0 {
		t.Fatalf("paused on unpriced usage: %+v", ag.PendingAsks())
	}
}

// A native CLI agent runs its own loop and answers for its own safety.
func TestLimits_NativeAgentsAreNeverLimited(t *testing.T) {
	native := &fakeNative{}
	ag, _ := limitAgent(t, native, true)
	ag.SetProvider(native, "cc")
	ag.SetMode(agent.ModeAutonomous)
	ag.SetLimits(agent.NewLimits(1, 0.001))
	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t1")); r.err != nil {
		t.Fatal(r.err)
	}
	for _, a := range ag.PendingAsks() {
		t.Fatalf("a native turn was paused: %+v", a)
	}
}

// With shell access disabled the model is not offered bash or terminal_*, and
// a call it makes anyway is refused with the reason.
func TestShellDisabled_WithholdsBashAndTerminals(t *testing.T) {
	p := &loopProvider{finishAfter: 1}
	shell := []tool.Tool{noopTool{"bash"}, noopTool{"terminal_start"}, noopTool{"terminal_output"}, noopTool{"terminal_stop"}, noopTool{"file_read"}}
	ag, _ := limitAgent(t, p, false, shell...)
	ag.SetShellDisabled(true)
	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t1")); r.err != nil {
		t.Fatal(r.err)
	}
	for _, name := range p.toolNames[0] {
		if name == "bash" || strings.HasPrefix(name, "terminal_") {
			t.Fatalf("offered %q with shell disabled: %v", name, p.toolNames[0])
		}
	}
	found := false
	for _, name := range p.toolNames[0] {
		found = found || name == "file_read"
	}
	if !found {
		t.Fatalf("other tools were withheld too: %v", p.toolNames[0])
	}

	// Back on: offered again.
	ag.SetShellDisabled(false)
	p.mu.Lock()
	p.calls = 0
	p.mu.Unlock()
	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t2")); r.err != nil {
		t.Fatal(r.err)
	}
	offered := strings.Join(p.toolNames[len(p.toolNames)-1], ",")
	if !strings.Contains(offered, "bash") || !strings.Contains(offered, "terminal_start") {
		t.Fatalf("shell tools not restored: %s", offered)
	}
}

type shellCallProvider struct{ calls int }

func (p *shellCallProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	p.calls++
	c, e := make(chan agent.ModelStreamChunk, 2), make(chan error, 1)
	if p.calls == 1 {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sh", Name: "terminal_start", Args: []byte(`{}`)}}
	} else {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ok"}
	}
	close(c)
	close(e)
	return c, e
}

func TestShellDisabled_RefusesADirectCall(t *testing.T) {
	ag, out := limitAgent(t, &shellCallProvider{}, false, noopTool{"terminal_start"})
	ag.SetShellDisabled(true)
	if r := waitTurn(t, runTurnAsync(ag, context.Background(), "t1")); r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(out.String(), "has no sandbox") {
		t.Fatalf("expected the refusal to say why, got: %s", out.String())
	}
}

// A registry applies both settings to sessions that already exist, and to new
// ones.
func TestRegistry_LimitsAndShellReachLiveSessions(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	reg := agent.NewRegistry(&loopProvider{}, "p", []tool.Tool{noopTool{"bash"}}, permission.New(norm, nil), snapStore, sessStore,
		agent.NewContextManager(1000, 100), nil, true)
	stream := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	live := reg.Get(stream, "live")
	t.Cleanup(reg.WaitBackground)

	reg.SetShellDisabled(true)
	if !live.ShellDisabled() || !reg.Get(stream, "fresh").ShellDisabled() {
		t.Fatal("shell policy did not reach both the live and a new session")
	}
	shared := agent.NewLimits(3, 2.5)
	reg.SetLimits(shared)
	shared.Set(7, 9)
	if r, c := live.LimitsForTest(); r != 7 || c != 9 {
		t.Fatalf("live session sees limits %d/%v, want 7/9", r, c)
	}
	if r, c := reg.Get(stream, "fresh").LimitsForTest(); r != 7 || c != 9 {
		t.Fatalf("new session sees limits %d/%v, want 7/9", r, c)
	}
}
