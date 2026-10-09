package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/audit"
)

// collector is the server's audit hook, for a test.
type collector struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *collector) add(e audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) of(kind string) []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []audit.Event
	for _, e := range c.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A native (CLI) agent's asks are audited the same way: answered by the mode
// (autonomous, Accept Edits, read-only) they are perm_auto; answered by a
// person they are perm_decision naming the device.
func TestAgent_NativeAsksAreAudited(t *testing.T) {
	ag, out := newTestAgent(t, t.TempDir(), &capturingProvider{}, nil)
	c := &collector{}
	ag.SetHooks(agent.Hooks{Audit: c.add})
	ctx := context.Background()

	ag.SetMode(agent.ModeAutonomous)
	_, _ = ag.AskNative(ctx, "s1", "t1", agent.NativeAsk{ToolCallID: "a1", ToolName: "Bash", Tool: "bash", Target: "make deploy"})
	ag.SetMode(agent.ModeAcceptEdits)
	_, _ = ag.AskNative(ctx, "s1", "t1", agent.NativeAsk{ToolCallID: "a2", ToolName: "Edit", Edit: true, Tool: "file_edit", Target: "main.go"})
	ag.SetMode(agent.ModeReadOnly)
	_, _ = ag.AskNative(ctx, "s1", "t1", agent.NativeAsk{ToolCallID: "a3", ToolName: "Bash", Tool: "bash", Target: "rm -rf /"})

	auto := c.of(audit.PermAuto)
	if len(auto) != 3 {
		t.Fatalf("want 3 perm_auto, got %+v", auto)
	}
	want := []struct{ outcome, via, tool, target string }{
		{"allow", "autonomous", "bash", "make deploy"},
		{"allow", "accept_edits", "file_edit", "main.go"},
		{"deny", "read_only", "bash", "rm -rf /"},
	}
	for i, w := range want {
		e := auto[i]
		if e.Outcome != w.outcome || e.Detail["via"] != w.via || e.Tool != w.tool || e.Target != w.target || e.Session != "s1" {
			t.Fatalf("perm_auto[%d] = %+v, want %+v", i, e, w)
		}
	}

	ag.SetMode(agent.ModeReadWrite)
	done := make(chan agent.NativeAnswer, 1)
	go func() {
		ans, _ := ag.AskNative(ctx, "s1", "t1", agent.NativeAsk{ToolCallID: "c1", ToolName: "Bash", Tool: "bash", Target: "git push"})
		done <- ans
	}()
	waitForSubstring(t, out, `"toolCallId":"c1"`, 2*time.Second)
	waitForAsk(t, ag, "c1", 2*time.Second)
	ag.HandlePermissionResponse("c1", true, true, "", agent.PermissionChoice{By: "tablet", RuleID: "agent"})
	if ans := <-done; !ans.Allowed {
		t.Fatalf("answer: %+v", ans)
	}
	dec := c.of(audit.PermDecision)
	if len(dec) != 1 || dec[0].Actor != "tablet" || dec[0].Outcome != "allow" || dec[0].Tool != "bash" || dec[0].Target != "git push" || dec[0].Session != "s1" || dec[0].Detail["remember"] != true {
		t.Fatalf("perm_decision: %+v", dec)
	}
	// A second answer to the same ask changes nothing and is not a decision.
	ag.HandlePermissionResponse("c1", false, false, "", agent.PermissionChoice{By: "tablet"})
	if n := len(c.of(audit.PermDecision)); n != 1 {
		t.Fatalf("a stale answer was logged as a decision (%d)", n)
	}
}

// A nil hook is what tests and -stdio have: nothing happens, nothing panics.
func TestAgent_AuditWithoutAHookIsANoOp(t *testing.T) {
	ag, _ := newTestAgent(t, t.TempDir(), &capturingProvider{}, nil)
	ag.SetMode(agent.ModeAutonomous)
	if ans, _ := ag.AskNative(context.Background(), "s1", "t1", agent.NativeAsk{ToolCallID: "a1", ToolName: "Bash"}); !ans.Allowed {
		t.Fatal("autonomous should allow")
	}
}
