package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// scriptProvider answers each request with whatever reply returns for it.
type scriptProvider struct {
	reply func(history []agent.Message, system string) []agent.ModelStreamChunk
}

func (p *scriptProvider) StreamTurn(_ context.Context, history []agent.Message, _ []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	chunks := p.reply(history, system)
	out := make(chan agent.ModelStreamChunk, len(chunks))
	for _, c := range chunks {
		out <- c
	}
	close(out)
	errs := make(chan error)
	close(errs)
	return out, errs
}

func callChunk(id, name string, args any) agent.ModelStreamChunk {
	b, _ := json.Marshal(args)
	return agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: name, Args: b}}
}

func textChunk(s string) agent.ModelStreamChunk {
	return agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: s}
}

func newFixesAgent(t *testing.T, p agent.ModelProvider, tools ...tool.Tool) (*agent.Agent, agent.SessionStore) {
	t.Helper()
	norm, _ := normalizer.New(t.TempDir())
	snapStore, _ := snapshot.NewDiskSnapshotStore(t.TempDir())
	sessStore, _ := agent.NewDiskSessionStore(t.TempDir())
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	ag := agent.NewAgent(ipc, p, "p", tools, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100000, 4000), nil)
	t.Cleanup(ag.WaitBackground)
	return ag, sessStore
}

// AGT-9: a blank prompt is refused before anything is stored.
func TestRunTurn_EmptyPromptRejected(t *testing.T) {
	ag, sessStore := newFixesAgent(t, &scriptProvider{func([]agent.Message, string) []agent.ModelStreamChunk { return nil }})
	if err := ag.RunTurn(context.Background(), "s", "t1", " \n"); err == nil {
		t.Fatal("blank prompt accepted")
	}
	if h, err := sessStore.Load("s"); err == nil && len(h.History) > 0 {
		t.Fatalf("blank prompt was persisted: %+v", h.History)
	}
}

// AGT-7: a subagent's own subagent call doesn't start a grandchild.
func TestSubagent_CannotSpawnSubagent(t *testing.T) {
	var mu sync.Mutex
	grandchild := false
	p := &scriptProvider{func(h []agent.Message, system string) []agent.ModelStreamChunk {
		switch first := h[0].Content; {
		case first == "grand":
			mu.Lock()
			grandchild = true
			mu.Unlock()
			return []agent.ModelStreamChunk{textChunk("grand report")}
		case first == "task" && len(h) == 1:
			return []agent.ModelStreamChunk{callChunk("g", agent.SubagentToolName, map[string]string{"description": "d", "prompt": "grand"})}
		case first == "task":
			return []agent.ModelStreamChunk{textChunk("report")}
		case len(h) == 1:
			return []agent.ModelStreamChunk{callChunk("s", agent.SubagentToolName, map[string]string{"description": "d", "prompt": "task"})}
		}
		return []agent.ModelStreamChunk{textChunk("done")}
	}}
	ag, _ := newFixesAgent(t, p)
	if err := ag.RunTurn(context.Background(), "s", "t1", "go"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if grandchild {
		t.Fatal("a subagent spawned a subagent")
	}
}

// cancelTool cancels the turn the first time it runs and counts runs.
type cancelTool struct {
	cancel context.CancelFunc
	runs   *atomic.Int32
}

func (cancelTool) Name() string                    { return "cancel_me" }
func (cancelTool) Description() string             { return "" }
func (cancelTool) Definition() tool.ToolDefinition { return tool.ToolDefinition{Name: "cancel_me"} }
func (cancelTool) RequiresPermission([]byte) bool  { return false }
func (c cancelTool) Execute(context.Context, tool.ExecutionContext, []byte) (string, error) {
	c.runs.Add(1)
	c.cancel()
	return "ok", nil
}

// AGT-2: after the turn is cancelled, the round's remaining calls don't run
// but each still gets a result, so the history stays paired.
func TestRunTurn_CancelSkipsRemainingToolCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := &atomic.Int32{}
	p := &scriptProvider{func(h []agent.Message, _ string) []agent.ModelStreamChunk {
		return []agent.ModelStreamChunk{callChunk("a", "cancel_me", nil), callChunk("b", "cancel_me", nil)}
	}}
	ag, sessStore := newFixesAgent(t, p, cancelTool{cancel, runs})
	_ = ag.RunTurn(ctx, "s", "t1", "go")
	if runs.Load() != 1 {
		t.Fatalf("tool ran %d times after cancel", runs.Load())
	}
	h, _ := sessStore.Load("s")
	results := map[string]string{}
	for _, m := range h.History {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if results["a"] != "ok" || !strings.Contains(results["b"], "cancelled") {
		t.Fatalf("results %v", results)
	}
}

// AGT-15: an unreadable usage.json is set aside, not overwritten.
func TestUsageStore_CorruptFileKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	os.WriteFile(path, []byte("{not json"), 0644)
	s, err := agent.NewUsageStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Record("s", 1, 1, 0.1)
	if b, err := os.ReadFile(path + ".corrupt"); err != nil || string(b) != "{not json" {
		t.Fatalf("corrupt copy: %q %v", b, err)
	}
}
