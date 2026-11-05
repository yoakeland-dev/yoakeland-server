package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
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

// stubTool is a tool that counts how often it really ran and always wants permission.
type stubTool struct {
	name string
	runs *atomic.Int32
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return s.name }
func (s stubTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{Name: s.name, Description: s.name, Parameters: map[string]any{"type": "object"}}
}
func (s stubTool) RequiresPermission([]byte) bool { return true }
func (s stubTool) Execute(context.Context, tool.ExecutionContext, []byte) (string, error) {
	s.runs.Add(1)
	return "ran", nil
}

func policyAgent(t *testing.T, p agent.ModelProvider, names ...string) (*agent.Agent, *syncBuf, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snaps, _ := snapshot.NewDiskSnapshotStore(dir)
	sess, _ := agent.NewDiskSessionStore(dir)
	runs := &atomic.Int32{}
	var tools []tool.Tool
	for _, n := range names {
		tools = append(tools, stubTool{n, runs})
	}
	out := &syncBuf{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), p, "test", tools, permission.New(norm, nil), snaps, sess, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)
	return ag, out, runs
}

func policyCall(id, name string) []agent.ModelStreamChunk {
	return []agent.ModelStreamChunk{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: name, Args: json.RawMessage(`{"command":"echo hi"}`)}}}
}

func policyText(s string) []agent.ModelStreamChunk {
	return []agent.ModelStreamChunk{{Kind: agent.StreamChunkDelta, Delta: s}}
}

var restricted = func() *agent.TurnPolicy {
	return &agent.TurnPolicy{FilesEdit: true, AllowCLI: func(string) bool { return false }, UserID: "u-1"}
}

// For a restricted policy bash, a terminal tool, mcp__* and the browser are left out of the tool
// list and refused if a model calls them anyway, with no permission prompt and no execution. The
// owner's turn (nil policy) still has all of them.
func TestTurnPolicy_WithholdsAndRefusesTools(t *testing.T) {
	all := []string{"bash", "terminal_start", "browser", "mcp__srv__do", "file_write"}
	script := func() [][]agent.ModelStreamChunk {
		var turns [][]agent.ModelStreamChunk
		var calls []agent.ModelStreamChunk
		for i, n := range []string{"bash", "terminal_start", "browser", "mcp__srv__do"} {
			calls = append(calls, policyCall("c"+string(rune('a'+i)), n)...)
		}
		return append(turns, calls, policyText("done"))
	}

	t.Run("restricted", func(t *testing.T) {
		prov := &capturingProvider{turns: script()}
		ag, out, runs := policyAgent(t, prov, all...)
		ag.SetMode(agent.ModeReadWrite)
		ctx := agent.WithTurnPolicy(context.Background(), restricted())
		if err := ag.RunTurn(ctx, "s1", "t1", "go"); err != nil {
			t.Fatal(err)
		}
		for _, n := range prov.toolNamesByTurn[0] {
			if n == "bash" || n == "terminal_start" || n == "browser" || strings.HasPrefix(n, "mcp__") {
				t.Errorf("tool %q was offered to a restricted turn: %v", n, prov.toolNamesByTurn[0])
			}
		}
		if !slices.Contains(prov.toolNamesByTurn[0], "file_write") {
			t.Errorf("an allowed tool went missing: %v", prov.toolNamesByTurn[0])
		}
		if runs.Load() != 0 {
			t.Errorf("a withheld tool ran %d times", runs.Load())
		}
		if strings.Contains(out.String(), `"permission_request"`) {
			t.Error("a withheld tool put a prompt to the user")
		}
		if n := strings.Count(out.String(), "isn't allowed for the person who started this turn"); n != 4 {
			t.Errorf("refusals = %d, want 4\n%s", n, out.String())
		}
	})

	t.Run("nil policy is exactly as before", func(t *testing.T) {
		prov := &capturingProvider{turns: script()}
		ag, out, _ := policyAgent(t, prov, all...)
		ag.SetMode(agent.ModeReadWrite)
		done := make(chan error, 1)
		go func() { done <- ag.RunTurn(context.Background(), "s1", "t1", "go") }()
		// Each call asks; deny them so the turn ends.
		for _, id := range []string{"ca", "cb", "cc", "cd"} {
			waitForAsk(t, ag, id, 5*time.Second)
			ag.HandlePermissionResponse(id, false, false, "")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"bash", "terminal_start", "browser", "mcp__srv__do", "file_write"} {
			if !slices.Contains(prov.toolNamesByTurn[0], n) {
				t.Errorf("owner turn lost tool %q", n)
			}
		}
		if strings.Contains(out.String(), "who started this turn") {
			t.Error("a policy refusal fired without a policy")
		}
	})

	t.Run("allowed by the policy", func(t *testing.T) {
		prov := &capturingProvider{turns: script()}
		ag, _, _ := policyAgent(t, prov, all...)
		ag.SetMode(agent.ModeReadWrite)
		p := restricted()
		p.Commands, p.Browser, p.MCP = true, true, true
		done := make(chan error, 1)
		go func() { done <- ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "go") }()
		for _, id := range []string{"ca", "cb", "cc", "cd"} {
			waitForAsk(t, ag, id, 5*time.Second)
			ag.HandlePermissionResponse(id, false, false, "")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"bash", "terminal_start", "browser", "mcp__srv__do"} {
			if !slices.Contains(prov.toolNamesByTurn[0], n) {
				t.Errorf("a permitted tool %q was withheld", n)
			}
		}
	})
}

// Autonomous is run as read_write, and no files_edit as read_only, for that turn only: the
// stored mode doesn't change. The permission prompt is what proves autonomous was not used.
func TestTurnPolicy_DowngradesModeWithoutChangingIt(t *testing.T) {
	t.Run("autonomous without the cap", func(t *testing.T) {
		prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{policyCall("c1", "bash"), policyText("done")}}
		ag, _, runs := policyAgent(t, prov, "bash", "file_write")
		ag.SetMode(agent.ModeAutonomous)
		p := restricted()
		p.Commands = true // bash is allowed, only autonomous is not
		done := make(chan error, 1)
		go func() { done <- ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "go") }()
		waitForAsk(t, ag, "c1", 5*time.Second) // autonomous would have run it unasked
		ag.HandlePermissionResponse("c1", false, false, "")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if runs.Load() != 0 {
			t.Error("bash ran unasked: the turn was still autonomous")
		}
		if ag.Mode() != agent.ModeAutonomous {
			t.Errorf("stored mode changed to %s", ag.Mode())
		}
	})

	t.Run("autonomous on a project with no sandbox", func(t *testing.T) {
		dir := t.TempDir()
		norm, _ := normalizer.New(dir)
		sess, _ := agent.NewDiskSessionStore(dir)
		runs := &atomic.Int32{}
		prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{policyCall("c1", "bash"), policyText("done")}}
		reg := agent.NewRegistry(prov, "test", []tool.Tool{stubTool{"bash", runs}}, permission.New(norm, nil), nil, sess, agent.NewContextManager(100, 20), nil, true)
		reg.SetUnsandboxed(true)
		out := &syncBuf{}
		ag := reg.Get(protocol.NewIPCStream(bytes.NewBuffer(nil), out), "s1")
		ag.SetMode(agent.ModeAutonomous)
		p := restricted()
		p.Commands, p.Unsandboxed, p.Autonomous = true, true, true
		done := make(chan error, 1)
		go func() { done <- ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "go") }()
		waitForAsk(t, ag, "c1", 5*time.Second)
		ag.HandlePermissionResponse("c1", false, false, "")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if runs.Load() != 0 {
			t.Error("autonomous ran an unsandboxed command")
		}
	})

	t.Run("commands are withheld on an unsandboxed project without the cap", func(t *testing.T) {
		dir := t.TempDir()
		norm, _ := normalizer.New(dir)
		sess, _ := agent.NewDiskSessionStore(dir)
		prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{policyText("done")}}
		reg := agent.NewRegistry(prov, "test", []tool.Tool{stubTool{"bash", &atomic.Int32{}}}, permission.New(norm, nil), nil, sess, agent.NewContextManager(100, 20), nil, true)
		reg.SetUnsandboxed(true)
		ag := reg.Get(protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuf{}), "s1")
		p := restricted()
		p.Commands = true // the sandboxed-project cap doesn't cover this project
		if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "go"); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(prov.toolNamesByTurn[0], "bash") {
			t.Error("bash was offered on an unsandboxed project with only the sandboxed cap")
		}
	})

	t.Run("no files_edit runs read_only", func(t *testing.T) {
		prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{policyText("done")}}
		ag, _, _ := policyAgent(t, prov, "file_read", "file_write", "bash")
		ag.SetMode(agent.ModeAcceptEdits)
		p := restricted()
		p.FilesEdit, p.Commands = false, true
		if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "go"); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(prov.toolNamesByTurn[0], "file_write") || slices.Contains(prov.toolNamesByTurn[0], "bash") || !slices.Contains(prov.toolNamesByTurn[0], "file_read") {
			t.Errorf("tools offered: %v", prov.toolNamesByTurn[0])
		}
		if ag.Mode() != agent.ModeAcceptEdits {
			t.Errorf("stored mode changed to %s", ag.Mode())
		}
	})
}

// A turn bound to a CLI agent the policy doesn't allow fails before it starts, naming the agent;
// a listed one runs, and so does an owner's turn.
func TestTurnPolicy_CLIAgents(t *testing.T) {
	cli := &fakeNative{}
	ag, out, _ := policyAgent(t, cli)
	ag.SetProvider(cli, "claude-cli")
	ag.SetMode(agent.ModeReadOnly) // the fake agent's ask is then answered without a person
	err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), restricted()), "s1", "t1", "hi")
	if err == nil || !strings.Contains(err.Error(), "claude-cli") {
		t.Fatalf("err = %v, want one naming the provider", err)
	}
	if len(cli.turns) != 0 {
		t.Fatal("the CLI agent was started")
	}
	if strings.Contains(out.String(), `"user_message"`) {
		t.Error("the refused prompt was recorded in the chat")
	}

	p := restricted()
	p.AllowCLI = func(n string) bool { return n == "claude-cli" }
	if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t2", "hi"); err != nil {
		t.Fatalf("a listed CLI agent was refused: %v", err)
	}
	if len(cli.turns) != 1 {
		t.Fatalf("turns = %d", len(cli.turns))
	}
	if err := ag.RunTurn(context.Background(), "s1", "t3", "hi"); err != nil {
		t.Fatalf("the owner's turn was refused: %v", err)
	}
}

// A CLI agent that is allowed still runs in the turn's effective mode, and without the MCP servers
// or browser it has no permission for.
func TestTurnPolicy_CLIAgentGetsEffectiveModeAndNoServers(t *testing.T) {
	cli := &fakeNative{}
	ag, _, _ := policyAgent(t, cli)
	ag.SetProvider(cli, "claude-cli")
	ag.SetMode(agent.ModeAutonomous)
	p := restricted()
	p.AllowCLI = func(string) bool { return true }
	if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), p), "s1", "t1", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := cli.turns[0].Mode; got != agent.ModeReadWrite {
		t.Errorf("CLI agent ran in %s, want read_write", got)
	}
	if len(cli.turns[0].MCP) != 0 {
		t.Errorf("MCP servers handed to a CLI agent without the cap: %v", cli.turns[0].MCP)
	}
	// Its own permission ask for a tool the policy withholds is refused without a prompt.
	if cli.answers[0].Allowed {
		t.Error("a Bash ask was allowed under a restricted policy")
	}
}

// A subagent gets its parent's policy: it isn't offered the withheld tools either.
func TestTurnPolicy_SubagentInherits(t *testing.T) {
	prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{
		{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sub", Name: agent.SubagentToolName, Args: json.RawMessage(`{"description":"d","prompt":"look around"}`)}}},
		policyText("report"),
		policyText("done"),
	}}
	ag, _, _ := policyAgent(t, prov, "bash", "browser", "mcp__srv__do", "file_read")
	ag.SetMode(agent.ModeReadWrite)
	if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), restricted()), "s1", "t1", "go"); err != nil {
		t.Fatal(err)
	}
	if len(prov.toolNamesByTurn) < 2 {
		t.Fatalf("the subagent never ran: %v", prov.toolNamesByTurn)
	}
	for _, n := range prov.toolNamesByTurn[1] {
		if n == "bash" || n == "browser" || strings.HasPrefix(n, "mcp__") {
			t.Errorf("subagent was offered %q: %v", n, prov.toolNamesByTurn[1])
		}
	}
}

// A subagent role that resolves to a CLI agent the policy doesn't allow is refused too.
func TestTurnPolicy_SubagentRoleCLIRefused(t *testing.T) {
	cli := &fakeNative{}
	prov := &capturingProvider{turns: [][]agent.ModelStreamChunk{
		{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "sub", Name: agent.SubagentToolName, Args: json.RawMessage(`{"description":"d","prompt":"look","kind":"explore"}`)}}},
		policyText("done"),
	}}
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	sess, _ := agent.NewDiskSessionStore(dir)
	reg := agent.NewRegistry(prov, "test", nil, permission.New(norm, nil), nil, sess, agent.NewContextManager(100, 20), nil, true)
	roles := agent.NewRoles()
	roles.Set(map[agent.Role]agent.RoleBinding{agent.RoleExplore: {Provider: cli, ProviderName: "claude-cli"}})
	reg.SetRoles(roles)
	out := &syncBuf{}
	ag := reg.Get(protocol.NewIPCStream(bytes.NewBuffer(nil), out), "s1")
	if err := ag.RunTurn(agent.WithTurnPolicy(context.Background(), restricted()), "s1", "t1", "go"); err != nil {
		t.Fatal(err)
	}
	if len(cli.turns) != 0 {
		t.Fatal("the explore role's CLI agent ran for a restricted turn")
	}
	if !strings.Contains(out.String(), "claude-cli") {
		t.Errorf("no refusal naming the agent: %s", out.String())
	}
}

// CancelTurnsWhere cancels the matching person's running turn and leaves others alone.
func TestCancelTurnsWhere(t *testing.T) {
	prov := &blockingProvider{started: make(chan struct{})}
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	sess, _ := agent.NewDiskSessionStore(dir)
	reg := agent.NewRegistry(prov, "test", nil, permission.New(norm, nil), nil, sess, agent.NewContextManager(100, 20), nil, true)
	ag := reg.Get(protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuf{}), "s1")
	ctx := agent.WithTurnActor(context.Background(), agent.TurnActor{UserID: "u-1", Credential: "phone"})
	done := make(chan error, 1)
	go func() { done <- ag.RunTurn(ctx, "s1", "t1", "go") }()
	<-prov.started
	if n := reg.CancelTurnsWhere(func(a agent.TurnActor) bool { return a.UserID == "someone-else" }); n != 0 {
		t.Fatalf("cancelled %d turns of another person", n)
	}
	if n := reg.CancelTurnsWhere(func(a agent.TurnActor) bool { return a.Credential == "phone" }); n != 1 {
		t.Fatalf("cancelled %d turns, want 1", n)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled turn kept running")
	}
}

// blockingProvider streams nothing until its context is cancelled.
type blockingProvider struct{ started chan struct{} }

func (b *blockingProvider) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk), make(chan error, 1)
	close(b.started)
	go func() {
		<-ctx.Done()
		close(c)
		e <- ctx.Err()
		close(e)
	}()
	return c, e
}
