package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"yoakeland/internal/agent"
)

// liveFake stands in for a CLI that stays running: every launch gets its own
// pipe pair, every line the provider writes is recorded, and script answers it.
type liveFake struct {
	mu       sync.Mutex
	launches [][]string
	got      []map[string]any
	closed   []chan struct{} // per launch: its stdin was closed
}

func (f *liveFake) starter(script func(launch int, msg map[string]any, out io.Writer)) procStarter {
	return func(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
		f.mu.Lock()
		idx := len(f.launches)
		f.launches = append(f.launches, append([]string{name}, args...))
		closed := make(chan struct{})
		f.closed = append(f.closed, closed)
		f.mu.Unlock()
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer outW.Close()
			defer close(closed)
			sc := bufio.NewScanner(inR)
			sc.Buffer(make([]byte, 64*1024), 10<<20)
			for sc.Scan() {
				var m map[string]any
				if json.Unmarshal(sc.Bytes(), &m) != nil {
					continue
				}
				f.mu.Lock()
				f.got = append(f.got, m)
				f.mu.Unlock()
				script(idx, m, outW)
			}
		}()
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64*1024), 10<<20)
		return &nativeProc{stdin: inW, lines: sc, stderr: func() string { return "" }, wait: func() error { <-done; return nil }}, nil
	}
}

func (f *liveFake) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches)
}

func (f *liveFake) args(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.launches[i], " ")
}

// wrote lists what the provider sent that has this key set to want (a
// control_request's subtype, a JSON-RPC method).
func (f *liveFake) wrote(match func(map[string]any) bool) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.got {
		if match(m) {
			out = append(out, m)
		}
	}
	return out
}

func controlSubtype(m map[string]any) string {
	r, _ := m["request"].(map[string]any)
	s, _ := r["subtype"].(string)
	return s
}

func (f *liveFake) controls(subtype string) []map[string]any {
	return f.wrote(func(m map[string]any) bool { return m["type"] == "control_request" && controlSubtype(m) == subtype })
}

func (f *liveFake) prompts() []string {
	var out []string
	for _, m := range f.wrote(func(m map[string]any) bool { return m["type"] == "user" }) {
		msg, _ := m["message"].(map[string]any)
		c, _ := msg["content"].([]any)
		b, _ := c[0].(map[string]any)
		out = append(out, b["text"].(string))
	}
	return out
}

func (f *liveFake) waitClosed(t *testing.T, launch int) {
	t.Helper()
	select {
	case <-f.closed[launch]:
	case <-time.After(5 * time.Second):
		t.Fatalf("launch %d was never closed", launch)
	}
}

// claudeScript is a Claude Code that answers each prompt with "reply N", its
// running cost, and acknowledges every control request.
func claudeScript(costPerTurn float64) func(int, map[string]any, io.Writer) {
	turns := 0
	return func(_ int, m map[string]any, out io.Writer) {
		switch m["type"] {
		case "user":
			turns++
			line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})
			line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "reply"}}})
			line(out, map[string]any{"type": "result", "subtype": "success", "result": "ok", "total_cost_usd": costPerTurn * float64(turns),
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		case "control_request":
			if id, _ := m["request_id"].(string); id != "thinking-display" && controlSubtype(m) != "interrupt" {
				line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id}})
			}
		}
	}
}

func liveTurn(key string, mode agent.Mode, resume, prompt string) *agent.NativeTurn {
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
	nt := rec.turn(mode)
	nt.SessionKey, nt.ResumeID, nt.Prompt = key, resume, prompt
	return nt
}

func TestClaudeSessionSurvivesTurns(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir(), Start: f.starter(claudeScript(0.10))}

	nt1 := liveTurn(key, agent.ModeReadWrite, "", "first")
	c1, err := runNative(t, p, nt1)
	if err != nil {
		t.Fatal(err)
	}
	if nt1.SessionID != "sess-1" || f.launchCount() != 1 {
		t.Fatalf("session %q, %d launches", nt1.SessionID, f.launchCount())
	}

	// Same chat, same settings: the process is reused, not resumed by a new one.
	c2, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "sess-1", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("second turn started a new process: %v", f.launches)
	}
	if got := strings.Join(f.prompts(), "|"); got != "first|second" {
		t.Fatalf("prompts = %s", got)
	}
	// total_cost_usd is the process's running total; a turn is billed its share.
	if u := c1[len(c1)-1].Usage; u.CostUSD < 0.0999 || u.CostUSD > 0.1001 {
		t.Fatalf("turn 1 cost = %v", u.CostUSD)
	}
	if u := c2[len(c2)-1].Usage; u.CostUSD < 0.0999 || u.CostUSD > 0.1001 {
		t.Fatalf("turn 2 cost = %v, want the delta", u.CostUSD)
	}

	// The permission mode and the model change in session.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAcceptEdits, "sess-1", "third")); err != nil {
		t.Fatal(err)
	}
	p.Model = "sonnet"
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAcceptEdits, "sess-1", "fourth")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("mode/model change restarted the process: %v", f.launches)
	}
	if m := f.controls("set_permission_mode"); len(m) != 1 || m[0]["request"].(map[string]any)["mode"] != "acceptEdits" {
		t.Fatalf("set_permission_mode = %v", m)
	}
	if m := f.controls("set_model"); len(m) != 1 || m[0]["request"].(map[string]any)["model"] != "sonnet" {
		t.Fatalf("set_model = %v", m)
	}

	// Effort is a launch argument: it restarts, resuming the same chat.
	p.effort = "high"
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAcceptEdits, "sess-1", "fifth")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "--effort high") || !strings.Contains(f.args(1), "--resume sess-1") {
		t.Fatalf("launches = %v", f.launches)
	}
	f.waitClosed(t, 0) // the old process was asked to leave

	// A history that moved on (no resume id) can't continue a live conversation.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAcceptEdits, "", "fresh")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 3 || strings.Contains(f.args(2), "--resume") {
		t.Fatalf("launches = %v", f.launches)
	}

	CloseNativeSessions(key)
	f.waitClosed(t, 2)
}

func TestClaudeSessionInterruptKeepsProcess(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := true
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["type"] == "user" && hang {
			line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})
			return // a turn that never finishes on its own
		}
		if m["type"] == "control_request" && controlSubtype(m) == "interrupt" {
			line(out, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": false})
			return
		}
		claudeScript(0.1)(l, m, out)
	})}

	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, liveTurn(key, agent.ModeReadWrite, "", "stop me")), nil, nil, "")
	go func() {
		for len(f.prompts()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	for range chunks {
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v", err)
	}
	if len(f.controls("interrupt")) != 1 {
		t.Fatalf("no interrupt was sent: %v", f.got)
	}
	hang = false
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "sess-1", "again")); err != nil {
		t.Fatalf("turn after interrupt: %v", err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("interrupt restarted the process: %v", f.launches)
	}
}

func TestClaudeSessionRelaunchesAfterDeath(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		claudeScript(0.1)(l, m, out)
		if l == 0 && m["type"] == "user" {
			_ = out.(*io.PipeWriter).Close() // the process dies right after its first turn
		}
	})}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "sess-1", "two")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "--resume sess-1") {
		t.Fatalf("launches = %v", f.launches)
	}
}
