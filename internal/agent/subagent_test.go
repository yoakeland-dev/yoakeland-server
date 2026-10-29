package agent_test

import (
	"bufio"
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

// subagentProvider answers by conversation: the parent's first round asks
// for two subagents, each subagent reports (B only after a file_write it
// must be granted), and the parent then finishes. Both subagents' first
// rounds wait for each other, so running them one after the other fails.
type subagentProvider struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	prompts []string // system prompts seen, to check the subagent's own
}

func (p *subagentProvider) StreamTurn(ctx context.Context, history []agent.Message, _ []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, system)
	p.mu.Unlock()
	out := make(chan agent.ModelStreamChunk, 4)
	errs := make(chan error, 1)
	send := func(chunks ...agent.ModelStreamChunk) {
		for _, c := range chunks {
			out <- c
		}
		close(out)
		close(errs)
	}
	call := func(id, name string, args any) agent.ModelStreamChunk {
		b, _ := json.Marshal(args)
		return agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: name, Args: b}}
	}
	text := func(s string) agent.ModelStreamChunk {
		return agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: s}
	}
	usage := agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}

	first := history[0].Content
	go func() {
		switch {
		case strings.HasPrefix(first, "task "):
			who := strings.TrimPrefix(first, "task ")
			if len(history) == 1 { // its first round: meet the other subagent
				p.mu.Lock()
				close(p.started[who])
				p.mu.Unlock()
				other := map[string]string{"A": "B", "B": "A"}[who]
				select {
				case <-p.started[other]:
				case <-time.After(5 * time.Second):
					send(text("the other subagent never started: they ran one at a time"))
					return
				}
				if who == "B" {
					send(call("b-write", "file_write", map[string]string{"path": "b.txt", "content": "hi"}), usage)
					return
				}
			}
			send(text("report "+who), usage)
		case len(history) == 1:
			send(
				call("sub-a", agent.SubagentToolName, map[string]string{"description": "do A", "prompt": "task A"}),
				call("sub-b", agent.SubagentToolName, map[string]string{"description": "do B", "prompt": "task B"}),
			)
		default:
			send(text("all done"))
		}
	}()
	return out, errs
}

// lineSink is an io.Writer the test reads envelopes back from as they're
// written.
type lineSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *lineSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}

func (s *lineSink) envelopes() []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.Envelope
	sc := bufio.NewScanner(bytes.NewReader(s.buf.Bytes()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var env protocol.Envelope
		if json.Unmarshal(sc.Bytes(), &env) == nil {
			out = append(out, env)
		}
	}
	return out
}

func TestSubagents_ParallelTaggedAndAskThroughParent(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(t.TempDir())
	sessStore, _ := agent.NewDiskSessionStore(t.TempDir())
	sink := &lineSink{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), sink)
	prov := &subagentProvider{started: map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{})}}
	ag := agent.NewAgent(ipc, prov, "p", []tool.Tool{tool.NewFileWriteTool(norm)}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100000, 4000), nil)
	t.Cleanup(ag.WaitBackground)

	// Answer subagent B's permission ask through the parent, as a client would.
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			for _, env := range sink.envelopes() {
				if env.Type == protocol.EventPermissionRequest {
					if env.ParentToolCallID != "sub-b" || env.ToolCallID != "b-write" {
						t.Errorf("permission_request tagged %q for %q", env.ParentToolCallID, env.ToolCallID)
					}
					ag.HandlePermissionResponse(env.ToolCallID, true, false, "")
					return
				}
			}
		}
	}()

	if err := ag.RunTurn(context.Background(), "s", "t1", "go"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	history, _ := sessStore.Load("s")
	results := map[string]agent.Message{}
	for _, m := range history.History {
		if m.Role == "tool" {
			results[m.ToolCallID] = m
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "report") {
			t.Fatalf("a subagent's message leaked into the parent's history: %+v", m)
		}
	}
	if results["sub-a"].Content != "report A" || results["sub-b"].Content != "report B" {
		t.Fatalf("subagent results: A=%q B=%q", results["sub-a"].Content, results["sub-b"].Content)
	}
	if len(results["sub-b"].Subagent) == 0 {
		t.Fatal("subagent B's transcript wasn't kept")
	}

	var tagged, completed int
	for _, env := range sink.envelopes() {
		if env.ParentToolCallID != "" {
			tagged++
		}
		if env.Type == protocol.EventTurnCompleted {
			if env.ParentToolCallID == "" {
				completed++
				var d protocol.TurnCompletedData
				_ = json.Unmarshal(env.Data, &d)
				if d.Usage.InputTokens != 30 { // A's one round + B's two, 10 each
					t.Errorf("turn usage %+v leaves out the subagents'", d.Usage)
				}
			}
		}
	}
	if tagged == 0 || completed != 1 {
		t.Fatalf("tagged events %d, untagged turn_completed %d", tagged, completed)
	}
	var sawSubagentPrompt bool
	for _, sp := range prov.prompts {
		sawSubagentPrompt = sawSubagentPrompt || sp == agent.SubagentSystemPrompt
	}
	if !sawSubagentPrompt {
		t.Fatal("subagents didn't get their own system prompt")
	}
}

// pingTool closes ran when executed.
type pingTool struct{ ran chan struct{} }

func (p pingTool) Name() string                    { return "ping" }
func (p pingTool) Description() string             { return "" }
func (p pingTool) Definition() tool.ToolDefinition { return tool.ToolDefinition{Name: "ping"} }
func (p pingTool) RequiresPermission([]byte) bool  { return false }
func (p pingTool) Execute(context.Context, tool.ExecutionContext, []byte) (string, error) {
	close(p.ran)
	return "pong", nil
}

// siblingProvider's parent asks for a subagent and a ping in one response;
// the subagent only reports once the ping ran, so holding the ping (or a
// question in its place) until the subagent finishes fails.
type siblingProvider struct{ ran chan struct{} }

func (p siblingProvider) StreamTurn(ctx context.Context, history []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	out := make(chan agent.ModelStreamChunk, 4)
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		defer close(out)
		switch {
		case history[0].Content == "task":
			select {
			case <-p.ran:
				out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "report"}
			case <-time.After(5 * time.Second):
				out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ping was held back"}
			}
		case len(history) == 1:
			b, _ := json.Marshal(map[string]string{"description": "d", "prompt": "task"})
			out <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sub", Name: agent.SubagentToolName, Args: b}}
			out <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "p", Name: "ping", Args: []byte("{}")}}
		default:
			out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "done"}
		}
	}()
	return out, errs
}

func TestSubagents_SiblingCallsRunMeanwhile(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(t.TempDir())
	sessStore, _ := agent.NewDiskSessionStore(t.TempDir())
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &lineSink{})
	ran := make(chan struct{})
	ag := agent.NewAgent(ipc, siblingProvider{ran}, "p", []tool.Tool{pingTool{ran}}, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100000, 4000), nil)
	ag.SetMode(agent.ModeAutonomous)
	t.Cleanup(ag.WaitBackground)
	if err := ag.RunTurn(context.Background(), "s", "t1", "go"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	history, _ := sessStore.Load("s")
	for _, m := range history.History {
		if m.Role == "tool" && m.ToolCallID == "sub" && m.Content != "report" {
			t.Fatalf("subagent result %q", m.Content)
		}
	}
}

// stuckSubagentProvider: the parent starts one subagent that never finishes on its own, then
// wraps up once that call has come back.
type stuckSubagentProvider struct{}

func (stuckSubagentProvider) StreamTurn(ctx context.Context, history []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	out := make(chan agent.ModelStreamChunk, 4)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		switch {
		case strings.HasPrefix(history[0].Content, "task"):
			<-ctx.Done()
		case len(history) == 1:
			args, _ := json.Marshal(map[string]string{"description": "stuck", "prompt": "task x"})
			out <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sub-x", Name: agent.SubagentToolName, Args: args}}
		default:
			out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "carried on"}
		}
	}()
	return out, errs
}

func TestSubagents_CancelStopsOnlyThatSubagent(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(t.TempDir())
	sessStore, _ := agent.NewDiskSessionStore(t.TempDir())
	sink := &lineSink{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), sink)
	ag := agent.NewAgent(ipc, stuckSubagentProvider{}, "p", nil, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100000, 4000), nil)
	ag.SetMode(agent.ModeAutonomous)
	t.Cleanup(ag.WaitBackground)

	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			for _, env := range sink.envelopes() {
				if env.Type == protocol.EventSubagentStatus && env.ParentToolCallID == "sub-x" {
					ag.CancelSubagent("sub-x")
					return
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- ag.RunTurn(context.Background(), "s", "t1", "go") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("turn never finished after cancelling the subagent")
	}
	history, _ := sessStore.Load("s")
	var stopped, final bool
	for _, m := range history.History {
		stopped = stopped || (m.Role == "tool" && m.ToolCallID == "sub-x" && strings.Contains(m.Content, "stopped by the user"))
		final = final || (m.Role == "assistant" && m.Content == "carried on")
	}
	if !stopped || !final {
		t.Fatalf("stopped=%v final=%v history=%+v", stopped, final, history.History)
	}
}
