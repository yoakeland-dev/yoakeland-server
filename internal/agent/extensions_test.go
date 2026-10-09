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
	"yoakeland/internal/skills"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// scriptedModel records what each request offered and replies from a script.
type scriptedModel struct {
	mu      sync.Mutex
	turns   [][]agent.ModelStreamChunk
	tools   [][]string
	systems []string
	history [][]agent.Message
}

func (m *scriptedModel) StreamTurn(_ context.Context, h []agent.Message, defs []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	m.tools, m.systems, m.history = append(m.tools, names), append(m.systems, system), append(m.history, append([]agent.Message(nil), h...))
	c, e := make(chan agent.ModelStreamChunk, 8), make(chan error, 1)
	if len(m.tools) <= len(m.turns) {
		for _, ch := range m.turns[len(m.tools)-1] {
			c <- ch
		}
	}
	close(c)
	close(e)
	return c, e
}

func callOf(id, name string, args any) []agent.ModelStreamChunk {
	b, _ := json.Marshal(args)
	return []agent.ModelStreamChunk{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: name, Args: b}}}
}

// fakeMCPTool is a tool from a source, like an MCP server's.
type fakeMCPTool struct {
	name     string
	readOnly bool
	ran      *atomic.Int32
}

func (f *fakeMCPTool) Name() string        { return f.name }
func (f *fakeMCPTool) Description() string { return "fake" }
func (f *fakeMCPTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{Name: f.name, Description: "fake", Parameters: map[string]any{"type": "object"}}
}
func (f *fakeMCPTool) RequiresPermission([]byte) bool { return true }
func (f *fakeMCPTool) ReadOnly() bool                 { return f.readOnly }
func (f *fakeMCPTool) Execute(context.Context, tool.ExecutionContext, []byte) (string, error) {
	f.ran.Add(1)
	return "ran " + f.name, nil
}

type fakeSource struct {
	tools    []tool.Tool
	prepared atomic.Int32
}

func (s *fakeSource) Tools() []tool.Tool      { return s.tools }
func (s *fakeSource) Prepare(context.Context) { s.prepared.Add(1) }

func extAgent(t *testing.T, model agent.ModelProvider, builtin ...tool.Tool) (*agent.Agent, *permission.Engine, *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	engine := permission.New(norm, nil)
	snap, _ := snapshot.NewDiskSnapshotStore(dir)
	sess, _ := agent.NewDiskSessionStore(dir)
	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, model, "test", builtin, engine, snap, sess, agent.NewContextManager(100000, 4000), nil)
	t.Cleanup(ag.WaitBackground)
	return ag, engine, out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestSource_ToolsAreOfferedPreparedAndAskedAbout(t *testing.T) {
	ran := &atomic.Int32{}
	src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}, &fakeMCPTool{name: "mcp__fake__look", readOnly: true, ran: ran}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "mcp__fake__write", map[string]any{"x": 1}), say("done")}}
	ag, _, out := extAgent(t, model)
	ag.SetMode(agent.ModeReadWrite)
	ag.SetToolSource(src)

	go func() {
		waitForAsk(t, ag, "c1", 5*time.Second)
		ag.HandlePermissionResponse("c1", true, false, "")
	}()
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if !has(model.tools[0], "mcp__fake__write") || !has(model.tools[0], "mcp__fake__look") {
		t.Fatalf("offered %v", model.tools[0])
	}
	if src.prepared.Load() < 2 {
		t.Fatalf("Prepare ran %d times; it runs before every request", src.prepared.Load())
	}
	if ran.Load() != 1 {
		t.Fatal("the allowed MCP call did not run")
	}
	if n := strings.Count(out.String(), `"type":"permission_request"`); n != 1 {
		t.Fatalf("an MCP tool must ask: %d permission requests", n)
	}
	if !strings.Contains(out.String(), `"mcp__fake__write"`) || !strings.Contains(out.String(), "ran mcp__fake__write") {
		t.Fatalf("events: %s", out.String())
	}
}

func TestSource_DeniedCallDoesNotRun(t *testing.T) {
	ran := &atomic.Int32{}
	src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "mcp__fake__write", nil), say("ok")}}
	ag, _, out := extAgent(t, model)
	ag.SetMode(agent.ModeReadWrite)
	ag.SetToolSource(src)
	go func() {
		waitForAsk(t, ag, "c1", 5*time.Second)
		ag.HandlePermissionResponse("c1", false, false, "no thanks")
	}()
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 || !strings.Contains(out.String(), "permission denied by user: no thanks") {
		t.Fatalf("ran=%d out=%s", ran.Load(), out.String())
	}
}

func TestSource_RuleAllowsWithoutAsking(t *testing.T) {
	ran := &atomic.Int32{}
	src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "mcp__fake__write", nil), say("ok")}}
	ag, engine, out := extAgent(t, model)
	ag.SetMode(agent.ModeReadWrite)
	ag.SetToolSource(src)
	if _, err := engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "mcp__fake__*", Pattern: []string{"*"}, Decision: permission.DecisionAllow}); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || strings.Contains(out.String(), "permission_request") {
		t.Fatalf("ran=%d, out=%s", ran.Load(), out.String())
	}
}

func TestSource_AutonomousRunsWithoutAsking(t *testing.T) {
	ran := &atomic.Int32{}
	src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "mcp__fake__write", nil), say("ok")}}
	ag, _, out := extAgent(t, model)
	ag.SetMode(agent.ModeAutonomous)
	ag.SetToolSource(src)
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || strings.Contains(out.String(), "permission_request") {
		t.Fatalf("ran=%d out=%s", ran.Load(), out.String())
	}
}

func TestSource_ReadOnlyModesOfferOnlyWhatServersCallReadOnly(t *testing.T) {
	for _, mode := range []agent.Mode{agent.ModeReadOnly, agent.ModePlan} {
		ran := &atomic.Int32{}
		src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}, &fakeMCPTool{name: "mcp__fake__look", readOnly: true, ran: ran}}}
		// A model that calls the withheld tool anyway (stale, or misbehaving).
		model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "mcp__fake__write", nil), say("ok")}}
		ag, _, out := extAgent(t, model)
		ag.SetMode(mode)
		ag.SetToolSource(src)
		if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
			t.Fatal(err)
		}
		if has(model.tools[0], "mcp__fake__write") || !has(model.tools[0], "mcp__fake__look") {
			t.Fatalf("%s: offered %v", mode, model.tools[0])
		}
		if ran.Load() != 0 || !strings.Contains(out.String(), "unavailable in the current mode") {
			t.Fatalf("%s: the withheld tool ran or wasn't refused: %s", mode, out.String())
		}
	}
}

// --- skills ---

type fakeSkills struct {
	list []skills.Skill
	docs map[string]tool.SkillDoc
}

func (f *fakeSkills) Skills(kind string) []skills.Skill {
	var out []skills.Skill
	for _, s := range f.list {
		if s.For(kind) && s.Active() {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeSkills) LoadSkill(name string, _ bool) (tool.SkillDoc, error) {
	if d, ok := f.docs[name]; ok {
		return d, nil
	}
	return tool.SkillDoc{}, context.DeadlineExceeded
}

func reviewSkill() skills.Skill {
	return skills.Skill{Name: "review", Description: "Review changes", Dir: "/skills/review", File: "/skills/review/SKILL.md", Agents: []string{skills.Native}, Enabled: true, UserInvocable: true}
}

func skillAgent(t *testing.T, model agent.ModelProvider, fs *fakeSkills) (*agent.Agent, *syncBuf) {
	ag, _, out := extAgent(t, model, tool.NewSkillTool(fs))
	ag.SetSkills(fs)
	return ag, out
}

func TestSkills_ListedInThePromptAndLoadedByTheTool(t *testing.T) {
	fs := &fakeSkills{list: []skills.Skill{reviewSkill()}, docs: map[string]tool.SkillDoc{"review": {Name: "review", Dir: "/skills/review", Body: "Check every changed file.", Files: []string{"checklist.md"}}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{callOf("c1", "skill", map[string]string{"name": "review"}), say("done")}}
	ag, out := skillAgent(t, model, fs)
	if err := ag.RunTurn(context.Background(), "s", "t", "please review"); err != nil {
		t.Fatal(err)
	}
	if !has(model.tools[0], "skill") {
		t.Fatalf("the skill tool wasn't offered: %v", model.tools[0])
	}
	if !strings.Contains(model.systems[0], "<name>review</name>") || !strings.Contains(model.systems[0], "/skills/review/SKILL.md") {
		t.Fatalf("system prompt lacks the listing:\n%s", model.systems[0])
	}
	if !strings.Contains(out.String(), "Check every changed file.") || !strings.Contains(out.String(), "checklist.md") {
		t.Fatalf("the skill's instructions never reached the model: %s", out.String())
	}
	if strings.Contains(out.String(), "permission_request") {
		t.Fatal("loading a skill must not ask")
	}
}

func TestSkills_NoSkillsMeansNoToolAndNoSection(t *testing.T) {
	fs := &fakeSkills{}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("hi")}}
	ag, _ := skillAgent(t, model, fs)
	if err := ag.RunTurn(context.Background(), "s", "t", "hello"); err != nil {
		t.Fatal(err)
	}
	if has(model.tools[0], "skill") || strings.Contains(model.systems[0], "# Skills") {
		t.Fatalf("offered tools %v; prompt has a skills section: %v", model.tools[0], strings.Contains(model.systems[0], "# Skills"))
	}
}

func TestSkills_AllowedInReadOnlyMode(t *testing.T) {
	fs := &fakeSkills{list: []skills.Skill{reviewSkill()}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("hi")}}
	ag, _ := skillAgent(t, model, fs)
	ag.SetMode(agent.ModeReadOnly)
	if err := ag.RunTurn(context.Background(), "s", "t", "hello"); err != nil {
		t.Fatal(err)
	}
	if !has(model.tools[0], "skill") {
		t.Fatalf("loading a skill reads files only, and read-only mode offers it: %v", model.tools[0])
	}
}

func TestSkills_ModelOnlyHiddenFromTheListWhenUserOnly(t *testing.T) {
	sk := reviewSkill()
	sk.DisableModelInvocation = true
	fs := &fakeSkills{list: []skills.Skill{sk}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("hi")}}
	ag, _ := skillAgent(t, model, fs)
	if err := ag.RunTurn(context.Background(), "s", "t", "hello"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(model.systems[0], "<name>review</name>") {
		t.Fatal("a skill only the user may start must not be offered to the model")
	}
}

func TestSkills_UserMentionLoadsTheSkillAsAToolCall(t *testing.T) {
	fs := &fakeSkills{list: []skills.Skill{reviewSkill()}, docs: map[string]tool.SkillDoc{"review": {Name: "review", Dir: "/skills/review", Body: "Check every changed file."}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("on it")}}
	ag, out := skillAgent(t, model, fs)
	if err := ag.RunTurn(context.Background(), "s", "t", "please $review the diff, it costs $5 and lives in $HOME and $nothere"); err != nil {
		t.Fatal(err)
	}
	h := model.history[0]
	var roles []string
	for _, m := range h {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool" {
		t.Fatalf("history sent to the model = %v (the named skill is loaded as a tool exchange)", roles)
	}
	if h[1].ToolCalls[0].Name != "skill" || !strings.Contains(h[2].Content, "Check every changed file.") {
		t.Fatalf("history = %+v", h)
	}
	if !strings.Contains(out.String(), `"tool_call_requested"`) || strings.Count(out.String(), `"tool_call_requested"`) != 1 {
		t.Fatalf("exactly one skill should have loaded: %s", out.String())
	}
	if h[0].Content != "please $review the diff, it costs $5 and lives in $HOME and $nothere" {
		t.Fatalf("the user's message is kept as typed: %q", h[0].Content)
	}
}

func TestSkills_MentionOfAUserOnlyOffSkillIsIgnored(t *testing.T) {
	sk := reviewSkill()
	sk.UserInvocable = false
	fs := &fakeSkills{list: []skills.Skill{sk}, docs: map[string]tool.SkillDoc{"review": {Name: "review", Body: "x"}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("ok")}}
	ag, _ := skillAgent(t, model, fs)
	_ = ag.RunTurn(context.Background(), "s", "t", "run $review")
	if len(model.history[0]) != 1 {
		t.Fatal("a skill that isn't user-invocable must not load from a mention")
	}
}

// A CLI agent is told to run a skill its own way.
type fakeCLI struct {
	fakeNative
	prompts []string
}

func (f *fakeCLI) SkillKind() string { return "claude" }
func (f *fakeCLI) SkillPrompt(prompt string, names []string) string {
	return "/" + names[0] + " " + strings.TrimSpace(strings.ReplaceAll(prompt, "$"+names[0], ""))
}
func (f *fakeCLI) StreamTurn(ctx context.Context, h []agent.Message, d []tool.ToolDefinition, s string) (<-chan agent.ModelStreamChunk, <-chan error) {
	f.prompts = append(f.prompts, agent.NativeTurnFromContext(ctx).Prompt)
	return f.fakeNative.StreamTurn(ctx, h, d, s)
}

func TestSkills_CLIAgentGetsItsOwnSyntaxAndOnlyItsOwnSkills(t *testing.T) {
	cli := &fakeCLI{}
	claudeOnly := skills.Skill{Name: "polish", Description: "d", Agents: []string{"claude"}, Enabled: true, UserInvocable: true}
	codexOnly := skills.Skill{Name: "ship", Description: "d", Agents: []string{"codex"}, Enabled: true, UserInvocable: true}
	fs := &fakeSkills{list: []skills.Skill{claudeOnly, codexOnly, reviewSkill()}}
	ag, _ := skillAgent(t, cli, fs)
	ag.SetMode(agent.ModeAutonomous)
	if err := ag.RunTurn(context.Background(), "s", "t", "$polish the README"); err != nil {
		t.Fatal(err)
	}
	if len(cli.prompts) != 1 || cli.prompts[0] != "/polish the README" {
		t.Fatalf("prompt sent to the CLI = %q", cli.prompts)
	}
	// A skill that agent can't load is left as typed.
	cli.prompts = nil
	if err := ag.RunTurn(context.Background(), "s", "t2", "$ship it"); err != nil {
		t.Fatal(err)
	}
	if cli.prompts[0] != "$ship it" {
		t.Fatalf("prompt = %q; a skill claude can't load must pass through", cli.prompts[0])
	}
}

func TestSkills_ContextBreakdownSizesMCPAndSkillsApart(t *testing.T) {
	fs := &fakeSkills{list: []skills.Skill{reviewSkill()}}
	ran := &atomic.Int32{}
	src := &fakeSource{tools: []tool.Tool{&fakeMCPTool{name: "mcp__fake__write", ran: ran}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("hi")}}
	ag, _ := skillAgent(t, model, fs)
	ag.SetToolSource(src)
	if err := ag.RunTurn(context.Background(), "s", "t", "hello"); err != nil {
		t.Fatal(err)
	}
	b := ag.ContextBreakdown(context.Background())
	keys := map[string]int{}
	for _, c := range b.Categories {
		keys[c.Key] = c.Tokens
	}
	if keys[protocol.CtxMCP] == 0 || keys[protocol.CtxSkills] == 0 {
		t.Fatalf("categories = %+v", b.Categories)
	}
}
