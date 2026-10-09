package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

type rulesRig struct {
	ag     *agent.Agent
	engine *permission.Engine
	out    *syncBuf
	dir    string
}

func newRulesRig(t *testing.T, mode agent.Mode, turns ...[]agent.ModelStreamChunk) *rulesRig {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	tools := []tool.Tool{tool.NewBashTool(norm, sandbox.NewSandbox(dir, 5*time.Second)), tool.NewFileWriteTool(norm), tool.NewFileEditTool(norm)}
	out := &syncBuf{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), &capturingProvider{turns: turns}, "p", tools, engine, snapStore, sessStore, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)
	ag.SetMode(mode)
	return &rulesRig{ag: ag, engine: engine, out: out, dir: norm.ProjectRoot}
}

func toolCall(id, name string, args map[string]string) []agent.ModelStreamChunk {
	b, _ := json.Marshal(args)
	return []agent.ModelStreamChunk{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: id, Name: name, Args: b}}}
}

func say(s string) []agent.ModelStreamChunk {
	return []agent.ModelStreamChunk{{Kind: agent.StreamChunkDelta, Delta: s}}
}

func (r *rulesRig) requests() int {
	return strings.Count(r.out.String(), `"type":"permission_request"`)
}

func (r *rulesRig) run(t *testing.T, turn string) {
	t.Helper()
	if err := r.ag.RunTurn(context.Background(), "s1", turn, "go"); err != nil {
		t.Fatalf("%s: %v", turn, err)
	}
}

// The root cause of the approval fatigue: bash used to be forced to ask in
// Ask/Edit/Auto whatever was remembered. An allow rule now answers first.
func TestRules_BashAllowRuleSkipsTheForcedAsk(t *testing.T) {
	for _, mode := range []agent.Mode{agent.ModeReadWrite, agent.ModeAcceptEdits, agent.ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			r := newRulesRig(t, mode, toolCall("c1", "bash", map[string]string{"command": "touch made.txt"}), say("ok"))
			if _, err := r.engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "bash", Pattern: []string{"touch", "*"}, Decision: permission.DecisionAllow}); err != nil {
				t.Fatal(err)
			}
			r.run(t, "t1")
			if r.requests() != 0 {
				t.Fatalf("asked despite an allow rule: %s", r.out.String())
			}
			if _, err := os.Stat(filepath.Join(r.dir, "made.txt")); err != nil {
				t.Fatalf("the command did not run: %v\n%s", err, r.out.String())
			}
		})
	}
}

func TestRules_ChainedBashStillAsksUnderAnAllowRule(t *testing.T) {
	r := newRulesRig(t, agent.ModeReadWrite, toolCall("c1", "bash", map[string]string{"command": "pwd; echo hi"}), say("ok"))
	_, _ = r.engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "bash", Pattern: []string{"pwd", "*"}, Decision: permission.DecisionAllow})
	go func() {
		waitForAsk(t, r.ag, "c1", 5*time.Second)
		r.ag.HandlePermissionResponse("c1", false, false, "")
	}()
	r.run(t, "t1")
	if r.requests() != 1 {
		t.Fatalf("expected an ask, got %d", r.requests())
	}
}

// Deny beats an allow, and beats Accept Edits letting edits through.
func TestRules_DenyWinsInAcceptEdits(t *testing.T) {
	r := newRulesRig(t, agent.ModeAcceptEdits,
		toolCall("c1", "file_write", map[string]string{"path": "secrets/key.txt", "content": "x"}),
		toolCall("c2", "file_write", map[string]string{"path": "notes.txt", "content": "y"}),
		say("ok"))
	_, _ = r.engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "edit", Pattern: []string{"secrets/**"}, Decision: permission.DecisionAllow})
	if _, err := r.engine.AddRule(permission.ScopeSession, permission.Rule{Tool: "edit", Pattern: []string{"secrets/**"}, Decision: permission.DecisionDeny}); err != nil {
		t.Fatal(err)
	}
	r.run(t, "t1")
	if r.requests() != 0 {
		t.Fatalf("asked: %s", r.out.String())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "secrets", "key.txt")); err == nil {
		t.Error("the denied write happened")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "notes.txt")); err != nil {
		t.Errorf("Accept Edits no longer lets an ordinary edit through: %v", err)
	}
	if !strings.Contains(r.out.String(), "permission denied by a session rule") {
		t.Errorf("the model was not told which rule denied it: %s", r.out.String())
	}
}

func TestRules_AskRuleBeatsAcceptEdits(t *testing.T) {
	r := newRulesRig(t, agent.ModeAcceptEdits, toolCall("c1", "file_write", map[string]string{"path": "keep/a.txt", "content": "x"}), say("ok"))
	_, _ = r.engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "edit", Pattern: []string{"keep/**"}, Decision: permission.DecisionAsk})
	go func() {
		waitForAsk(t, r.ag, "c1", 5*time.Second)
		r.ag.HandlePermissionResponse("c1", false, false, "")
	}()
	r.run(t, "t1")
	if r.requests() != 1 {
		t.Fatalf("expected the ask rule to ask, got %d requests", r.requests())
	}
}

// Picking a suggestion stores that rule (not the exact one), in the scope
// asked for, and it then covers a different call.
func TestRules_PickedSuggestionIsRemembered(t *testing.T) {
	r := newRulesRig(t, agent.ModeReadWrite,
		toolCall("c1", "file_write", map[string]string{"path": "pkg/a.txt", "content": "1"}), say("one"),
		toolCall("c2", "file_write", map[string]string{"path": "pkg/sub/b.txt", "content": "2"}), say("two"),
		toolCall("c3", "file_write", map[string]string{"path": "other/c.txt", "content": "3"}), say("three"))
	go func() {
		waitForAsk(t, r.ag, "c1", 5*time.Second)
		r.ag.HandlePermissionResponse("c1", true, false, "", agent.PermissionChoice{RuleID: "dir", Scope: "session"})
	}()
	r.run(t, "t1")
	r.run(t, "t2")
	if r.requests() != 1 {
		t.Fatalf("a sibling under the same directory asked again (%d)", r.requests())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "pkg", "sub", "b.txt")); err != nil {
		t.Errorf("not written: %v", err)
	}
	rules := r.engine.Rules()
	if len(rules) != 1 || rules[0].Scope != permission.ScopeSession || rules[0].Pattern[0] != "pkg/**" || rules[0].Source != permission.SourceSuggested {
		t.Fatalf("rules = %+v", rules)
	}
	// Somewhere else still asks.
	go func() {
		waitForAsk(t, r.ag, "c3", 5*time.Second)
		r.ag.HandlePermissionResponse("c3", false, false, "")
	}()
	r.run(t, "t3")
	if r.requests() != 2 {
		t.Fatalf("expected a second ask outside the directory, got %d", r.requests())
	}
}

// An answer naming a suggestion the server never offered stores nothing.
func TestRules_UnknownRuleIDIsIgnored(t *testing.T) {
	r := newRulesRig(t, agent.ModeReadWrite, toolCall("c1", "bash", map[string]string{"command": "pwd"}), say("ok"))
	go func() {
		waitForAsk(t, r.ag, "c1", 5*time.Second)
		r.ag.HandlePermissionResponse("c1", true, false, "", agent.PermissionChoice{RuleID: "everything", Scope: "global"})
	}()
	r.run(t, "t1")
	if n := len(r.engine.Rules()); n != 0 {
		t.Fatalf("stored %d rules from an unknown id", n)
	}
}

// Denying with remember stores a deny rule for the exact call (legacy behaviour).
func TestRules_DeniedAndRememberedStaysDenied(t *testing.T) {
	r := newRulesRig(t, agent.ModeReadWrite,
		toolCall("c1", "bash", map[string]string{"command": "pwd"}), say("one"),
		toolCall("c2", "bash", map[string]string{"command": "pwd"}), say("two"))
	go func() {
		waitForAsk(t, r.ag, "c1", 5*time.Second)
		r.ag.HandlePermissionResponse("c1", false, true, "")
	}()
	r.run(t, "t1")
	r.run(t, "t2")
	if r.requests() != 1 {
		t.Fatalf("a remembered deny asked again (%d)", r.requests())
	}
	if !strings.Contains(r.out.String(), "permission denied by a project rule") {
		t.Fatalf("second call not refused by the rule: %s", r.out.String())
	}
}

// Autonomous is the one bypass: the engine is never consulted.
func TestRules_AutonomousIgnoresRules(t *testing.T) {
	r := newRulesRig(t, agent.ModeAutonomous, toolCall("c1", "bash", map[string]string{"command": "touch made.txt"}), say("ok"))
	_, _ = r.engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "bash", Pattern: []string{"touch", "made.txt"}, Decision: permission.DecisionDeny})
	r.run(t, "t1")
	if _, err := os.Stat(filepath.Join(r.dir, "made.txt")); err != nil || r.requests() != 0 {
		t.Fatalf("autonomous did not just run the command: %v\n%s", err, r.out.String())
	}
}

// --- the native bridge: yoake deny rules only, the agent's own suggestions ---

type askingNative struct {
	ask agent.NativeAsk
	ans chan agent.NativeAnswer
}

func (f *askingNative) RunsOwnTools() {}
func (f *askingNative) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk, 2), make(chan error, 1)
	nt := agent.NativeTurnFromContext(ctx)
	go func() {
		defer close(c)
		defer close(e)
		ans, _ := nt.Ask(ctx, f.ask)
		f.ans <- ans
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "done"}
	}()
	return c, e
}

func nativeRig(t *testing.T, mode agent.Mode, ask agent.NativeAsk) (*agent.Agent, *permission.Engine, *askingNative, *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	nat := &askingNative{ask: ask, ans: make(chan agent.NativeAnswer, 1)}
	out := &syncBuf{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), nat, "cc", nil, engine, snapStore, sessStore, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)
	ag.SetMode(mode)
	return ag, engine, nat, out
}

func TestRules_NativeAskRefusedByDenyRuleOnly(t *testing.T) {
	ask := agent.NativeAsk{ToolCallID: "n1", ToolName: "Bash", Description: "git push", Tool: "bash", Target: "/usr/bin/git -C . push origin main"}
	ag, engine, nat, out := nativeRig(t, agent.ModeReadWrite, ask)
	if _, err := engine.AddRule(permission.ScopeProject, permission.Rule{Tool: "bash", Pattern: []string{"git", "push", "*"}, Decision: permission.DecisionDeny}); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	ans := <-nat.ans
	if ans.Allowed || !strings.Contains(ans.Feedback, "permission denied by a project rule") {
		t.Fatalf("answer = %+v", ans)
	}
	if strings.Contains(out.String(), "permission_request") {
		t.Fatalf("a denied native ask was still put to the user")
	}

	// An allow rule does not answer a native ask: it still goes to the user.
	ask2 := agent.NativeAsk{ToolCallID: "n2", ToolName: "Bash", Tool: "bash", Target: "ls"}
	ag2, engine2, nat2, _ := nativeRig(t, agent.ModeReadWrite, ask2)
	_, _ = engine2.AddRule(permission.ScopeProject, permission.Rule{Tool: "bash", Pattern: []string{"ls", "*"}, Decision: permission.DecisionAllow})
	go func() {
		waitForAsk(t, ag2, "n2", 5*time.Second)
		ag2.HandlePermissionResponse("n2", true, false, "")
	}()
	if err := ag2.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ans := <-nat2.ans; !ans.Allowed {
		t.Fatalf("answer = %+v", ans)
	}
}

func TestRules_NativeSuggestionsAreLabelledAndThePickReachesTheProvider(t *testing.T) {
	ask := agent.NativeAsk{ToolCallID: "n1", ToolName: "Bash", Description: "npm test", Suggestions: []agent.NativeSuggestion{
		{ID: "native:0", Label: "Always allow Bash(npm test:*)"}, {ID: "native:1", Label: "Switch Claude to acceptEdits"},
	}}
	ag, _, nat, _ := nativeRig(t, agent.ModeReadWrite, ask)
	go func() {
		waitForAsk(t, ag, "n1", 5*time.Second)
		var got []protocol.PermissionSuggestion
		for _, p := range ag.PendingAsks() {
			got = p.PermissionRequestData.Suggestions
		}
		if len(got) != 2 || !got[0].Native || got[1].Label != "Switch Claude to acceptEdits" {
			t.Errorf("suggestions = %+v", got)
		}
		ag.HandlePermissionResponse("n1", true, true, "", agent.PermissionChoice{RuleID: "native:1"})
	}()
	if err := ag.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ans := <-nat.ans; !ans.Allowed || !ans.Remember || ans.RuleID != "native:1" {
		t.Fatalf("answer = %+v", ans)
	}

	// With none offered there is one generic option, and picking it is a plain remember.
	ag2, _, nat2, _ := nativeRig(t, agent.ModeReadWrite, agent.NativeAsk{ToolCallID: "n2", ToolName: "shell"})
	go func() {
		waitForAsk(t, ag2, "n2", 5*time.Second)
		for _, p := range ag2.PendingAsks() {
			if s := p.PermissionRequestData.Suggestions; len(s) != 1 || s[0].ID != agent.NativeSuggestionAgent || !s[0].Native {
				t.Errorf("default suggestions = %+v", s)
			}
		}
		ag2.HandlePermissionResponse("n2", true, false, "", agent.PermissionChoice{RuleID: agent.NativeSuggestionAgent})
	}()
	if err := ag2.RunTurn(context.Background(), "s", "t", "go"); err != nil {
		t.Fatal(err)
	}
	if ans := <-nat2.ans; !ans.Allowed || !ans.Remember || ans.RuleID != "" {
		t.Fatalf("answer = %+v", ans)
	}
}
