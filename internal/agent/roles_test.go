package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// roleProvider answers with reply(history) and records each call's tools.
type roleProvider struct {
	name  string
	reply func(history []agent.Message) agent.ModelStreamChunk

	mu    sync.Mutex
	calls [][]string
}

func (p *roleProvider) ModelName() string { return p.name + "-model" }

func (p *roleProvider) GenerateTitle(context.Context, string) (string, error) {
	return p.name + " title", nil
}

func (p *roleProvider) StreamTurn(_ context.Context, history []agent.Message, tools []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	var names []string
	for _, td := range tools {
		names = append(names, td.Name)
	}
	p.mu.Lock()
	p.calls = append(p.calls, names)
	p.mu.Unlock()
	out, errs := make(chan agent.ModelStreamChunk, 1), make(chan error)
	out <- p.reply(history)
	close(out)
	close(errs)
	return out, errs
}

func (p *roleProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func text(s string) agent.ModelStreamChunk {
	return agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: s}
}

// Plan mode runs on the plan role, explore subagents on the explore role
// (read-only), titles on the utility role, and everything else on the
// session's own model.
func TestRoles_RouteWorkToTheirModels(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	tools := []tool.Tool{tool.NewFileReadTool(norm), tool.NewSearchTool(norm), tool.NewFileWriteTool(norm), tool.NewWritePlanTool(t.TempDir())}

	subCall, _ := json.Marshal(map[string]string{"description": "look", "prompt": "look around", "kind": "explore"})
	main := &roleProvider{name: "main", reply: func([]agent.Message) agent.ModelStreamChunk { return text("main done") }}
	plan := &roleProvider{name: "plan", reply: func(h []agent.Message) agent.ModelStreamChunk {
		if h[len(h)-1].Role == "tool" {
			return text("plan done")
		}
		return agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sub1", Name: agent.SubagentToolName, Args: subCall}}
	}}
	explore := &roleProvider{name: "explore", reply: func([]agent.Message) agent.ModelStreamChunk { return text("found it") }}
	utility := &roleProvider{name: "utility", reply: func([]agent.Message) agent.ModelStreamChunk { return text("") }}

	roles := agent.NewRoles()
	roles.Set(map[agent.Role]agent.RoleBinding{
		agent.RolePlan:    {Provider: plan, ProviderName: "p", Model: plan.ModelName()},
		agent.RoleExplore: {Provider: explore, ProviderName: "e", Model: explore.ModelName()},
		agent.RoleUtility: {Provider: utility, ProviderName: "u", Model: utility.ModelName()},
	})
	reg := agent.NewRegistry(main, "m", tools, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(64000, 4000), nil, false)
	reg.SetRoles(roles)
	ag := reg.Get(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), "s1")
	t.Cleanup(ag.WaitBackground)

	ag.SetMode(agent.ModePlan)
	if err := ag.RunTurn(context.Background(), "s1", "t1", "plan it"); err != nil {
		t.Fatal(err)
	}
	if main.callCount() != 0 || plan.callCount() != 2 || explore.callCount() != 1 {
		t.Fatalf("plan turn: calls main=%d plan=%d explore=%d, want 0/2/1", main.callCount(), plan.callCount(), explore.callCount())
	}
	for _, name := range explore.calls[0] {
		if name == "file_write" || name == agent.SubagentToolName {
			t.Fatalf("explore subagent was given %q; it must be read-only and can't nest: %v", name, explore.calls[0])
		}
	}

	ag.SetMode(agent.ModeReadWrite)
	if err := ag.RunTurn(context.Background(), "s1", "t2", "do it"); err != nil {
		t.Fatal(err)
	}
	if main.callCount() != 1 {
		t.Fatalf("read/write turn should run on the session's own model, got %d main calls", main.callCount())
	}

	ag.WaitBackground()
	state, err := sessStore.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Title != "utility title" {
		t.Fatalf("title should come from the utility role, got %q", state.Title)
	}
	if m := state.History[1]; m.Model != "plan-model" {
		t.Fatalf("plan turn's message should be stamped with the plan model, got %q", m.Model)
	}
}

// Past the budget, old tool results are stubbed (the latest round's kept)
// and the compaction is recorded in history.
func TestCompaction_ClearsOldToolResults(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	line := strings.Repeat("0123456789", 5) + "\n"
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat(line, 400)), 0o644)
	}
	read := func(id, path string) agent.ModelStreamChunk {
		args, _ := json.Marshal(map[string]string{"path": path})
		return agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: "file_read", Args: args}}
	}
	provider := &HistoryRecordingProvider{ScriptedProvider: ScriptedProvider{turns: [][]agent.ModelStreamChunk{
		{read("c1", "a.txt")}, {read("c2", "b.txt")}, {read("c3", "c.txt")}, {text("done")},
	}}}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), provider, "p",
		[]tool.Tool{tool.NewFileReadTool(norm)}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(8000, 4000), nil)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "read them"); err != nil {
		t.Fatal(err)
	}
	last := provider.seenHistory[3]
	var results []string
	for _, m := range last {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if len(results) != 3 || !strings.HasPrefix(results[0], "[cleared") || !strings.HasPrefix(results[1], "[cleared") || strings.HasPrefix(results[2], "[cleared") {
		t.Fatalf("expected the two older results cleared and the latest kept, got %d results: %.40q", len(results), results)
	}
	state, _ := sessStore.Load("s1")
	found := false
	for _, m := range state.History {
		if m.Role == "compaction" && m.Compaction != nil && m.Compaction.Kind == agent.CompactionClear {
			found = true
		}
		if m.Role == "tool" && strings.HasPrefix(m.Content, "[cleared") {
			t.Fatal("compaction must not rewrite stored history, only the view sent to the model")
		}
	}
	if !found {
		t.Fatal("expected a clear compaction recorded in history")
	}
}
