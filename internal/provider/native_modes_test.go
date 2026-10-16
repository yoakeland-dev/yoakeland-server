package provider

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/agent"
)

func TestPickACPMode(t *testing.T) {
	gemini := []acpModeChoice{{"default", "Default"}, {"autoEdit", "Auto Edit"}, {"yolo", "YOLO"}, {"plan", "Plan"}}
	for m, want := range map[agent.Mode]string{
		agent.ModeReadWrite: "default", agent.ModeAcceptEdits: "autoEdit", agent.ModeAutonomous: "yolo", agent.ModePlan: "plan",
	} {
		if got, ok := pickACPMode(gemini, m); !ok || got != want {
			t.Errorf("%s -> %q %v, want %q", m, got, ok, want)
		}
	}
	// "auto" is a classifier on some agents, not a bypass; only opencode's two modes exist here.
	if id, ok := pickACPMode([]acpModeChoice{{"auto", "Auto"}}, agent.ModeAutonomous); ok {
		t.Errorf("autonomous matched %q", id)
	}
	opencode := []acpModeChoice{{"build", "Build"}, {"plan", "Plan"}}
	if _, ok := pickACPMode(opencode, agent.ModeAutonomous); ok {
		t.Error("opencode has no autonomous mode")
	}
	specs := acpModes(opencode)
	byMode := map[string]ModeSpec{}
	for _, s := range specs {
		byMode[s.Mode] = s
	}
	if byMode["autonomous"].EnforcedBy != "yoakeland" || byMode["plan"].EnforcedBy != "agent" || byMode["plan"].Plan != "native" {
		t.Errorf("opencode specs = %+v", byMode)
	}
	if s := acpModes(nil); s[len(s)-1].Plan != "emulated" {
		t.Errorf("an agent with no modes plans by instruction: %+v", s[len(s)-1])
	}
}

func TestACPSessionModeChoices(t *testing.T) {
	var s acpSession
	_ = json.Unmarshal([]byte(`{"sessionId":"s","configOptions":[{"id":"permission-mode","category":"mode","currentValue":"ask","options":[{"value":"ask","name":"Ask"},{"value":"plan","name":"Plan"}]}]}`), &s)
	choices, cfg, cur := s.modeChoices()
	if cfg != "permission-mode" || cur != "ask" || len(choices) != 2 {
		t.Fatalf("config-option modes = %v %q %q", choices, cfg, cur)
	}
	s = acpSession{}
	_ = json.Unmarshal([]byte(`{"modes":{"currentModeId":"default","availableModes":[{"id":"default","name":"Default"}]}}`), &s)
	if choices, cfg, cur = s.modeChoices(); cfg != "" || cur != "default" || len(choices) != 1 {
		t.Fatalf("modes block = %v %q %q", choices, cfg, cur)
	}
}

func TestPermissionModesOnEveryModel(t *testing.T) {
	models := withPermissionModes([]ModelInfo{{ID: "m", Capabilities: map[string]any{"reasoningEfforts": []string{"low"}}}}, claudeModes(false))
	if models[0].Capabilities["reasoningEfforts"] == nil {
		t.Fatal("existing capabilities must survive")
	}
	specs := models[0].Capabilities["permissionModes"].([]ModeSpec)
	if len(specs) != 5 || specs[2].Mode != "accept_edits" || specs[2].Native != "acceptEdits" || specs[2].Policy["edit"] != "allow" || specs[2].Policy["run"] != "ask" {
		t.Fatalf("claude specs = %+v", specs)
	}
	if !specs[3].Sandbox || specs[4].Plan != "native" {
		t.Fatalf("claude autonomous/plan = %+v %+v", specs[3], specs[4])
	}
}

// Plan mode: Claude's own plan file is written without asking, and asking to
// leave plan mode reaches the user as a plan approval.
func TestClaudePlanMode(t *testing.T) {
	home, _ := os.UserHomeDir()
	planFile := filepath.Join(home, ".claude", "plans", "x.md")
	var args []string
	var replies []map[string]any
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		in.Scan()
		line(out, map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": planFile, "content": "# p"}, "tool_use_id": "w1"}})
		in.Scan()
		var m map[string]any
		_ = json.Unmarshal(in.Bytes(), &m)
		replies = append(replies, m)
		line(out, map[string]any{"type": "control_request", "request_id": "r2", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": "/etc/passwd", "content": "x"}, "tool_use_id": "w2"}})
		in.Scan()
		_ = json.Unmarshal(in.Bytes(), &m)
		line(out, map[string]any{"type": "control_request", "request_id": "r3", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "ExitPlanMode", "input": map[string]any{"plan": "# The plan"}, "tool_use_id": "x1"}})
		in.Scan()
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "done"})
	})
	if _, err := runNative(t, p, rec.turn(agent.ModePlan)); err != nil {
		t.Fatal(err)
	}
	wait()
	if !strings.Contains(strings.Join(args, " "), "--permission-mode plan") {
		t.Fatalf("args = %v", args)
	}
	if len(replies) != 1 || len(rec.asks) != 2 {
		t.Fatalf("the plan file write must not ask, the other two must: %d replies, asks %+v", len(replies), rec.asks)
	}
	if rec.asks[0].ToolName != "Write" || rec.asks[0].Plan != "" || !rec.asks[0].Edit {
		t.Fatalf("outside write ask = %+v", rec.asks[0])
	}
	if rec.asks[1].Plan != "# The plan" {
		t.Fatalf("exit plan ask = %+v", rec.asks[1])
	}
}

func TestCodexPolicyAcceptEdits(t *testing.T) {
	if sb, ap := codexPolicy(agent.ModeAcceptEdits); sb != "workspace-write" || ap != "untrusted" {
		t.Fatalf("accept_edits -> %s %s", sb, ap)
	}
	if claudePermissionMode(agent.ModeAcceptEdits) != "acceptEdits" {
		t.Fatal("claude accept_edits")
	}
}

// Ultracode goes to Claude as xhigh plus its own setting; fast mode is a setting too.
func TestClaudeEffortAndFastMode(t *testing.T) {
	for _, c := range []struct {
		effort, want string
		fast         bool
	}{
		{"ultracode", `--effort xhigh --settings {"fastMode":true,"ultracode":true}`, true},
		{"max", "--effort max", false},
		{"default", "", false},
	} {
		var args []string
		p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir()}
		yes := c.fast
		p.SetModelOptions(agent.ModelOptions{ReasoningEffort: c.effort, FastMode: &yes})
		var wait func()
		p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
			in.Scan()
			line(out, map[string]any{"type": "result", "subtype": "success", "result": "ok"})
		})
		if _, err := runNative(t, p, (&turnRecorder{}).turn(agent.ModeReadWrite)); err != nil {
			t.Fatal(err)
		}
		wait()
		got := strings.Join(args, " ")
		if !strings.Contains(got, c.want) || (c.want == "" && (strings.Contains(got, "--effort") || strings.Contains(got, "--settings"))) {
			t.Errorf("effort %q fast %v: args = %s", c.effort, c.fast, got)
		}
	}
}

// Auto is offered only where the agent has a reviewer to do the deciding.
func TestAutoModeOnlyWhereTheAgentHasAReviewer(t *testing.T) {
	names := func(specs []ModeSpec) string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Mode)
		}
		return strings.Join(out, ",")
	}
	if got := names(claudeModes(false)); got != "read_only,read_write,accept_edits,autonomous,plan" {
		t.Fatalf("claude without auto = %s", got)
	}
	specs := claudeModes(true)
	if got := names(specs); got != "read_only,read_write,accept_edits,auto,autonomous,plan" {
		t.Fatalf("claude with auto = %s", got)
	}
	if a := specs[3]; a.Native != "auto" || a.EnforcedBy != "agent" || a.Policy["run"] != "auto" || a.Policy["edit"] != "allow" {
		t.Fatalf("claude auto spec = %+v", a)
	}
	if got := names(codexModes()); !strings.Contains(got, ",auto,") {
		t.Fatalf("codex = %s", got)
	}
	if got := names(apiModes()); strings.Contains(got, "auto,") && strings.Contains(got, ",auto") {
		t.Fatalf("api models offer auto: %s", got)
	}
	if got := names(acpModes(nil)); strings.Contains(got, ",auto,") {
		t.Fatalf("acp offers auto: %s", got)
	}

	// Per model: only the ones that report support.
	models := withPermissionModesBy([]ModelInfo{
		{ID: "opus", Capabilities: map[string]any{"autoMode": true}},
		{ID: "haiku"},
	}, func(m ModelInfo) []ModeSpec {
		auto, _ := m.Capabilities["autoMode"].(bool)
		return claudeModes(auto)
	})
	if n := len(models[0].Capabilities["permissionModes"].([]ModeSpec)); n != 6 {
		t.Fatalf("opus modes = %d", n)
	}
	if n := len(models[1].Capabilities["permissionModes"].([]ModeSpec)); n != 5 {
		t.Fatalf("haiku modes = %d", n)
	}
}

// Auto is sandboxed on Codex (workspace-write) and not on Claude.
func TestAutoSandboxFlag(t *testing.T) {
	auto := func(specs []ModeSpec) ModeSpec {
		for _, s := range specs {
			if s.Mode == "auto" {
				return s
			}
		}
		t.Fatal("no auto spec")
		return ModeSpec{}
	}
	if !auto(codexModes()).Sandbox || auto(claudeModes(true)).Sandbox {
		t.Fatal("codex auto should be sandboxed, claude auto not")
	}
}
