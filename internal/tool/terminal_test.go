//go:build unix

package tool

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/terminal"
)

// shellDir is a workspace for a test that leaves a shell running. HOME is the
// workspace, so bash writes .bash_history into it as it's killed, racing
// TempDir's removal; retry the removal instead.
func shellDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "terminal-tools")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50 && os.RemoveAll(dir) != nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	})
	return dir
}

// TestTerminalTools_StartReadStop drives the agent's terminal tools end to
// end: a command runs in a real shell, finishes, its output comes back
// cleaned, input typed later runs too, and only the agent's own tab can be
// stopped.
func TestTerminalTools_StartReadStop(t *testing.T) {
	dir := shellDir(t)
	norm, _ := normalizer.New(dir)
	m := terminal.NewManager(sandbox.NewSandbox(dir, 60*time.Second), norm)
	defer m.CloseSession("s")
	ctx := context.Background()
	ec := ExecutionContext{SessionID: "s"}
	args := func(v any) []byte { b, _ := json.Marshal(v); return b }

	out, err := NewTerminalStartTool(m).Execute(ctx, ec, args(map[string]any{
		"command": "printf 'a\\rHELLO-%s\\n' $((6*7))", "name": "greeter", "wait_ms": 10000,
	}))
	if err != nil {
		t.Fatalf("terminal_start: %v", err)
	}
	if !strings.Contains(out, "terminal agent-1 (greeter)") || !strings.Contains(out, "HELLO-42") {
		t.Fatalf("terminal_start output:\n%s", out)
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "aHELLO") {
		t.Fatalf("output not cleaned:\n%q", out)
	}
	if summ := m.List("s"); len(summ) != 1 || !summ[0].Agent || summ[0].Name != "greeter" {
		t.Fatalf("list = %+v", summ)
	}

	out, err = NewTerminalOutputTool(m).Execute(ctx, ec, args(map[string]any{"id": "agent-1", "input": "echo SECOND\n", "wait_ms": 10000}))
	if err != nil || !strings.Contains(out, "SECOND") || strings.Contains(out, "HELLO-42") {
		t.Fatalf("terminal_output = %q, %v", out, err)
	}

	if _, err := m.Open(ctx, nil, "s", "term-user", "", 80, 24, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTerminalStopTool(m).Execute(ctx, ec, args(map[string]any{"id": "term-user"})); err == nil {
		t.Fatal("stopped the user's own terminal")
	}
	if _, err := NewTerminalStopTool(m).Execute(ctx, ec, args(map[string]any{"id": "agent-1"})); err != nil {
		t.Fatalf("terminal_stop: %v", err)
	}
	if _, err := m.State("s", "agent-1"); err == nil {
		t.Fatal("agent-1 still open after terminal_stop")
	}
}

// With the project's web tools off, an agent's terminal is cut off from the
// network like bash is; only loopback is left in its namespace.
func TestTerminalStart_NoNetworkWhenAgentNetworkOff(t *testing.T) {
	dir := shellDir(t)
	norm, _ := normalizer.New(dir)
	sb := sandbox.NewSandbox(dir, 60*time.Second)
	if sb.Mode() != sandbox.ModeBubblewrap {
		t.Skip("needs bubblewrap")
	}
	m := terminal.NewManager(sb, norm)
	defer m.CloseSession("s")
	ifaces := func() string {
		b, _ := json.Marshal(map[string]any{"command": "echo IF: $(tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | tr '\\n' ,)", "wait_ms": 10000})
		out, err := NewTerminalStartTool(m).Execute(context.Background(), ExecutionContext{SessionID: "s"}, b)
		if err != nil {
			t.Fatal(err)
		}
		i := strings.LastIndex(out, "IF: ")
		if i < 0 {
			t.Fatalf("no interface list in:\n%s", out)
		}
		return strings.Fields(out[i+4:])[0]
	}
	m.SetAgentNetwork(false)
	if got := ifaces(); got != "lo," {
		t.Fatalf("agent terminal sees interfaces %q with network off", got)
	}
	m.SetAgentNetwork(true)
	if got := ifaces(); got == "lo," {
		t.Skip("host has no interface besides loopback")
	}
}
