package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"reflect"
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
	nt.SessionKey, nt.ResumeID, nt.Prompt, nt.FreshPrompt = key, resume, prompt, prompt
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

// codexScript is a Codex app-server that finishes every turn with "reply".
// While hang is set it starts turns without finishing them, and answers an
// interrupt by ending the turn.
func codexScript(hang *bool) func(int, map[string]any, io.Writer) {
	turns, threads := 0, 0
	var current string
	reply := func(out io.Writer, m map[string]any, result any) {
		line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result})
	}
	finish := func(out io.Writer, status string) {
		line(out, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 10, "outputTokens": 3}}}})
		line(out, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": current, "status": status}}})
	}
	return func(_ int, m map[string]any, out io.Writer) {
		switch m["method"] {
		case "initialize", "account/rateLimits/read":
			reply(out, m, map[string]any{})
		case "thread/start", "thread/resume":
			threads++
			id := "th" + string(rune('0'+threads))
			if p, _ := m["params"].(map[string]any); p["threadId"] != nil {
				id = p["threadId"].(string)
			}
			reply(out, m, map[string]any{"thread": map[string]any{"id": id}})
		case "turn/start":
			turns++
			current = "turn" + string(rune('0'+turns))
			reply(out, m, map[string]any{"turn": map[string]any{"id": current}})
			if !*hang {
				line(out, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "reply"}})
				finish(out, "completed")
			}
		case "turn/interrupt":
			reply(out, m, map[string]any{})
			finish(out, "interrupted")
		}
	}
}

func (f *liveFake) rpc(method string) []map[string]any {
	return f.wrote(func(m map[string]any) bool { return m["method"] == method })
}

func TestCodexSessionSurvivesTurns(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Model: "gpt-5.5", Dir: t.TempDir(), Start: f.starter(codexScript(&hang))}

	nt1 := liveTurn(key, agent.ModeReadWrite, "", "first")
	if _, err := runNative(t, p, nt1); err != nil {
		t.Fatal(err)
	}
	if nt1.SessionID != "th1" {
		t.Fatalf("session = %q", nt1.SessionID)
	}
	// Same chat, a different mode, model and effort: one process, one thread.
	p.Model, p.effort = "gpt-6-luna", "high"
	nt2 := liveTurn(key, agent.ModeAutonomous, "th1", "second")
	if _, err := runNative(t, p, nt2); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 || len(f.rpc("thread/start")) != 1 || len(f.rpc("thread/resume")) != 0 {
		t.Fatalf("launches %d, thread/start %d, thread/resume %d", f.launchCount(), len(f.rpc("thread/start")), len(f.rpc("thread/resume")))
	}
	turns := f.rpc("turn/start")
	if len(turns) != 2 {
		t.Fatalf("turn/start calls = %d", len(turns))
	}
	first, second := turns[0]["params"].(map[string]any), turns[1]["params"].(map[string]any)
	if first["approvalPolicy"] != "untrusted" || first["sandboxPolicy"].(map[string]any)["type"] != "workspaceWrite" {
		t.Fatalf("first turn policy = %v", first)
	}
	if second["approvalPolicy"] != "never" || second["model"] != "gpt-6-luna" || second["effort"] != "high" {
		t.Fatalf("second turn = %v", second)
	}
	// A history that moved on starts a new thread, in the same process.
	nt3 := liveTurn(key, agent.ModeReadWrite, "", "fresh")
	if _, err := runNative(t, p, nt3); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 || len(f.rpc("thread/start")) != 2 || nt3.SessionID != "th2" {
		t.Fatalf("launches %d, thread/start %d, session %q", f.launchCount(), len(f.rpc("thread/start")), nt3.SessionID)
	}
	CloseNativeSessions(key)
	f.waitClosed(t, 0)
}

func TestCodexSessionInterruptKeepsProcess(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := true
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), Start: f.starter(codexScript(&hang))}

	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, liveTurn(key, agent.ModeReadWrite, "", "stop me")), nil, nil, "")
	go func() {
		for len(f.rpc("turn/start")) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // the reply to turn/start
		cancel()
	}()
	for range chunks {
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v", err)
	}
	if len(f.rpc("turn/interrupt")) != 1 {
		t.Fatalf("no turn/interrupt was sent")
	}
	hang = false
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "th1", "again")); err != nil {
		t.Fatalf("turn after interrupt: %v", err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("interrupt restarted the process: %v", f.launches)
	}
}

// acpScript is an ACP agent with model and effort options. While hang is set a
// prompt stays open until the client cancels it.
func acpScript(hang *bool) func(int, map[string]any, io.Writer) {
	sessions := 0
	var pending any
	update := func(out io.Writer, u map[string]any) {
		line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s", "update": u}})
	}
	reply := func(out io.Writer, id, result any) {
		line(out, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	return func(_ int, m map[string]any, out io.Writer) {
		switch m["method"] {
		case "initialize":
			reply(out, m["id"], map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
		case "session/new":
			sessions++
			reply(out, m["id"], map[string]any{"sessionId": "s" + string(rune('0'+sessions)), "configOptions": []any{
				map[string]any{"id": "model", "category": "model", "currentValue": "a", "options": []any{}},
				map[string]any{"id": "effort", "category": "thought_level", "currentValue": "low", "options": []any{}},
			}})
		case "session/set_config_option":
			reply(out, m["id"], map[string]any{})
		case "session/prompt":
			if *hang {
				pending = m["id"]
				return
			}
			update(out, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "reply"}})
			reply(out, m["id"], map[string]any{"stopReason": "end_turn"})
		case "session/cancel":
			if pending != nil {
				reply(out, pending, map[string]any{"stopReason": "cancelled"})
				pending = nil
			}
		}
	}
}

func TestACPSessionSurvivesTurns(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Model: "m1", effort: "high", Dir: t.TempDir(), Start: f.starter(acpScript(&hang))}

	nt1 := liveTurn(key, agent.ModeReadWrite, "", "first")
	if _, err := runNative(t, p, nt1); err != nil {
		t.Fatal(err)
	}
	if nt1.SessionID != "s1" {
		t.Fatalf("session = %q", nt1.SessionID)
	}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "s1", "second")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 || len(f.rpc("session/new")) != 1 || len(f.rpc("session/load"))+len(f.rpc("session/resume")) != 0 {
		t.Fatalf("launches %d, new %d", f.launchCount(), len(f.rpc("session/new")))
	}
	if n := len(f.rpc("session/set_config_option")); n != 2 { // model and effort, once
		t.Fatalf("config options set %d times, want 2", n)
	}
	if got := strings.Join(promptTexts(f), "|"); got != "first|second" {
		t.Fatalf("prompts = %s", got)
	}

	// Only a change is sent.
	p.Model = "m2"
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "s1", "third")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.rpc("session/set_config_option")); n != 3 {
		t.Fatalf("config options set %d times, want 3", n)
	}

	// A history that moved on gets a new session in the same process, with its options set again.
	nt4 := liveTurn(key, agent.ModeReadWrite, "", "fresh")
	if _, err := runNative(t, p, nt4); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 || len(f.rpc("session/new")) != 2 || nt4.SessionID != "s2" || len(f.rpc("session/set_config_option")) != 5 {
		t.Fatalf("launches %d, new %d, session %q, options %d", f.launchCount(), len(f.rpc("session/new")), nt4.SessionID, len(f.rpc("session/set_config_option")))
	}
	CloseNativeSessions(key)
	f.waitClosed(t, 0)
}

func promptTexts(f *liveFake) []string {
	var out []string
	for _, m := range f.rpc("session/prompt") {
		prompt := m["params"].(map[string]any)["prompt"].([]any)
		out = append(out, prompt[0].(map[string]any)["text"].(string))
	}
	return out
}

func TestACPSessionCancelWaitsForThePrompt(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := true
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Dir: t.TempDir(), Start: f.starter(acpScript(&hang))}

	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, liveTurn(key, agent.ModeReadWrite, "", "stop me")), nil, nil, "")
	go func() {
		for len(f.rpc("session/prompt")) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	for range chunks {
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v", err)
	}
	if len(f.rpc("session/cancel")) != 1 {
		t.Fatal("no session/cancel was sent")
	}
	hang = false
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "s1", "again")); err != nil {
		t.Fatalf("turn after cancel: %v", err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("cancel restarted the process: %v", f.launches)
	}
}

// steerAfterPrompt steers nt once the fake has seen a prompt, and reports the error.
func steerAfterPrompt(nt *agent.NativeTurn, seen func() bool, text string, result chan<- error) {
	go func() {
		for !seen() {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // the turn's own bookkeeping after the prompt goes out
		result <- nt.Steer(text)
	}()
}

func TestClaudeSteerFoldsIntoTheRunningTurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	prompts := 0
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		prompts++
		line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})
		if prompts == 1 {
			return // working; the steer arrives while it does
		}
		// One result ends the whole exchange, steer included.
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "ok", "usage": map[string]any{}})
	})}
	nt := liveTurn(key, agent.ModeReadWrite, "", "first")
	steered := make(chan error, 1)
	steerAfterPrompt(nt, func() bool { return len(f.prompts()) > 0 }, "also this", steered)
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	if err := <-steered; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if got := strings.Join(f.prompts(), "|"); got != "first|also this" {
		t.Fatalf("prompts = %s", got)
	}
	if err := nt.Steer("too late"); err == nil {
		t.Fatal("a finished turn took a steer")
	}
}

func TestCodexSteerNamesTheTurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := true
	script := codexScript(&hang)
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["method"] == "turn/steer" {
			line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"turnId": "turn1"}})
			line(out, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "turn1", "status": "completed"}}})
			return
		}
		script(l, m, out)
	})}
	nt := liveTurn(key, agent.ModeReadWrite, "", "first")
	steered := make(chan error, 1)
	steerAfterPrompt(nt, func() bool { return len(f.rpc("turn/start")) > 0 }, "also this", steered)
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	if err := <-steered; err != nil {
		t.Fatalf("steer: %v", err)
	}
	st := f.rpc("turn/steer")
	if len(st) != 1 {
		t.Fatalf("turn/steer calls = %d", len(st))
	}
	params := st[0]["params"].(map[string]any)
	input := params["input"].([]any)[0].(map[string]any)
	if params["expectedTurnId"] != "turn1" || params["threadId"] != "th1" || input["text"] != "also this" {
		t.Fatalf("turn/steer params = %v", params)
	}
}

// A steered ACP prompt supersedes the one before it, which some agents answer
// "cancelled": only the last prompt's answer may settle the turn.
func TestACPSteerLastPromptSettles(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	var firstID any
	prompts := 0
	base := acpScript(new(bool))
	p := &CLISubprocessProvider{Kind: CLIKindCursor, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["method"] != "session/prompt" {
			base(l, m, out)
			return
		}
		prompts++
		if prompts == 1 {
			firstID = m["id"]
			return
		}
		line(out, map[string]any{"jsonrpc": "2.0", "id": firstID, "result": map[string]any{"stopReason": "cancelled"}})
		line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "both"}}}})
		line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"stopReason": "end_turn"}})
	})}
	nt := liveTurn(key, agent.ModeReadWrite, "", "first")
	steered := make(chan error, 1)
	steerAfterPrompt(nt, func() bool { return len(f.rpc("session/prompt")) > 0 }, "also this", steered)
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatalf("the superseded prompt's \"cancelled\" ended the turn: %v", err)
	}
	if err := <-steered; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if got := strings.Join(promptTexts(f), "|"); got != "first|also this" {
		t.Fatalf("prompts = %s", got)
	}
	if k := kinds(chunks); !strings.Contains(k, "delta") {
		t.Fatalf("chunks = %s", k)
	}
}

func TestClaudeAutoMode(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	refuse := false
	script := claudeScript(0.1)
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if refuse && m["type"] == "control_request" && controlSubtype(m) == "set_permission_mode" {
			line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": m["request_id"], "error": "auto mode unavailable for this model"}})
			return
		}
		script(l, m, out)
	})}
	// Never a launch argument (a model without it would take the flag and run unreviewed):
	// the session starts in default and is switched, which is refused where auto isn't available.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAuto, "", "one")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.args(0), "--permission-mode default") || strings.Contains(f.args(0), "--permission-mode auto") {
		t.Fatalf("args = %s", f.args(0))
	}
	// Moving into and out of it in a running session is a control request.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "sess-1", "two")); err != nil {
		t.Fatal(err)
	}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAuto, "sess-1", "three")); err != nil {
		t.Fatal(err)
	}
	var modes []any
	for _, m := range f.controls("set_permission_mode") {
		modes = append(modes, m["request"].(map[string]any)["mode"])
	}
	if !reflect.DeepEqual(modes, []any{"auto", "default", "auto"}) || f.launchCount() != 1 {
		t.Fatalf("mode changes = %v, %d launches", modes, f.launchCount())
	}
	// A model that can't do auto says so, and the turn fails rather than running unreviewed.
	refuse = true
	_, err := runNative(t, p, liveTurn(key, agent.ModeAcceptEdits, "sess-1", "four"))
	if err == nil {
		t.Fatal("expected the refused mode change to fail the turn")
	}
}

func TestCodexAutoReviewIsPerTurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), Start: f.starter(codexScript(&hang))}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAuto, "", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "th1", "two")); err != nil {
		t.Fatal(err)
	}
	start := f.rpc("thread/start")[0]["params"].(map[string]any)
	if start["approvalsReviewer"] != "auto_review" || start["approvalPolicy"] != "on-request" || start["sandbox"] != "workspace-write" {
		t.Fatalf("thread/start = %v", start)
	}
	turns := f.rpc("turn/start")
	one, two := turns[0]["params"].(map[string]any), turns[1]["params"].(map[string]any)
	if one["approvalsReviewer"] != "auto_review" || one["approvalPolicy"] != "on-request" {
		t.Fatalf("auto turn = %v", one)
	}
	// The reviewer sticks across turns unless told otherwise, so leaving Auto says "user".
	if two["approvalsReviewer"] != "user" || two["approvalPolicy"] != "untrusted" {
		t.Fatalf("turn after auto = %v", two)
	}
}

func TestClaudeForksAtACheckpoint(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		id := "sess-1"
		if l > 0 {
			id = "forked"
		}
		line(out, map[string]any{"type": "system", "subtype": "init", "session_id": id})
		line(out, map[string]any{"type": "assistant", "uuid": "a-1", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hi"}}}})
		line(out, map[string]any{"type": "assistant", "uuid": "a-2", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hi again"}}}})
		line(out, map[string]any{"type": "result", "subtype": "success", "usage": map[string]any{}})
	})}
	nt1 := liveTurn(key, agent.ModeReadWrite, "", "first")
	if _, err := runNative(t, p, nt1); err != nil {
		t.Fatal(err)
	}
	if nt1.Checkpoint != "a-2" {
		t.Fatalf("checkpoint = %q, want the turn's last assistant message", nt1.Checkpoint)
	}

	// Going back to it: a new process forked from that point, whose own id is what the chat follows.
	nt2 := liveTurn(key, agent.ModeReadWrite, "sess-1", "edited")
	nt2.ForkAt = "a-2"
	if _, err := runNative(t, p, nt2); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "--resume sess-1 --fork-session --resume-session-at a-2") {
		t.Fatalf("launches = %v", f.launches)
	}
	if nt2.SessionID != "forked" {
		t.Fatalf("session = %q, want the fork's own id", nt2.SessionID)
	}
	// And the next turn continues on the fork.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "forked", "after")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 {
		t.Fatalf("the fork was restarted: %v", f.launches)
	}
}

func TestClaudeForkFailureFallsBackToTheTranscript(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if l == 0 && m["type"] == "user" {
			_ = out.(*io.PipeWriter).Close() // no such message: it exits without a word
			return
		}
		claudeScript(0.1)(l, m, out)
	})}
	nt := liveTurn(key, agent.ModeReadWrite, "sess-1", "edited")
	nt.ForkAt, nt.FreshPrompt = "gone", "TRANSCRIPT edited"
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatalf("a failed fork should fall back, not fail: %v", err)
	}
	if f.launchCount() != 2 || strings.Contains(f.args(1), "--resume") || strings.Contains(f.args(1), "--fork-session") {
		t.Fatalf("launches = %v", f.launches)
	}
	if got := f.prompts(); got[len(got)-1] != "TRANSCRIPT edited" {
		t.Fatalf("prompts = %v", got)
	}
}

func TestCodexForksAtATurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	script := codexScript(&hang)
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["method"] == "thread/fork" {
			line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"thread": map[string]any{"id": "forked"}}})
			return
		}
		script(l, m, out)
	})}
	nt1 := liveTurn(key, agent.ModeReadWrite, "", "first")
	if _, err := runNative(t, p, nt1); err != nil {
		t.Fatal(err)
	}
	if nt1.Checkpoint != "turn1" {
		t.Fatalf("checkpoint = %q", nt1.Checkpoint)
	}
	// The same live thread, cut back: a fork through that turn, not a resume of it.
	nt2 := liveTurn(key, agent.ModeReadWrite, "th1", "edited")
	nt2.ForkAt = "turn1"
	if _, err := runNative(t, p, nt2); err != nil {
		t.Fatal(err)
	}
	forks := f.rpc("thread/fork")
	if len(forks) != 1 || f.launchCount() != 1 {
		t.Fatalf("forks %d, launches %d", len(forks), f.launchCount())
	}
	fp := forks[0]["params"].(map[string]any)
	if fp["threadId"] != "th1" || fp["lastTurnId"] != "turn1" {
		t.Fatalf("thread/fork = %v", fp)
	}
	if nt2.SessionID != "forked" {
		t.Fatalf("session = %q", nt2.SessionID)
	}
	turns := f.rpc("turn/start")
	if turns[1]["params"].(map[string]any)["threadId"] != "forked" {
		t.Fatalf("the turn ran on %v, not the fork", turns[1]["params"])
	}
}

func TestCodexForkFailureFallsBackToTheTranscript(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	script := codexScript(&hang)
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["method"] == "thread/fork" {
			line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32600, "message": "no rollout found"}})
			return
		}
		script(l, m, out)
	})}
	nt := liveTurn(key, agent.ModeReadWrite, "old", "edited")
	nt.ForkAt, nt.FreshPrompt = "turn9", "TRANSCRIPT edited"
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatalf("a failed fork should fall back: %v", err)
	}
	starts := f.rpc("thread/start")
	if len(starts) != 1 || starts[0]["params"].(map[string]any)["lastTurnId"] != nil {
		t.Fatalf("thread/start = %v", starts)
	}
	in := f.rpc("turn/start")[0]["params"].(map[string]any)["input"].([]any)[0].(map[string]any)
	if in["text"] != "TRANSCRIPT edited" {
		t.Fatalf("turn input = %v", in)
	}
}

// ampScript is an Amp that ends every turn with an end_turn assistant message,
// after a tool call whose stop reason is tool_use (mid-turn), like the real one.
func ampScript(hang *bool) func(int, map[string]any, io.Writer) {
	return func(_ int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "T-1"})
		if *hang {
			return
		}
		u := map[string]any{"input_tokens": 5, "output_tokens": 2}
		line(out, map[string]any{"type": "assistant", "message": map[string]any{"stop_reason": "tool_use", "usage": u, "content": []any{
			map[string]any{"type": "thinking", "thinking": "hmm"},
			map[string]any{"type": "text", "text": "Looking."},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}}})
		line(out, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "a.txt"}}}})
		line(out, map[string]any{"type": "assistant", "message": map[string]any{"stop_reason": "end_turn", "usage": u, "content": []any{map[string]any{"type": "text", "text": "Found a.txt."}}}})
	}
}

func TestAmpTurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	p := &CLISubprocessProvider{Kind: CLIKindAmp, Model: "medium", Dir: t.TempDir(), Start: f.starter(ampScript(&hang))}

	nt := liveTurn(key, agent.ModeAutonomous, "", "list files")
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatal(err)
	}
	// A tool_use stop reason is mid-turn; only end_turn ends it (there is no result message).
	if k := kinds(chunks); k != "reasoning,delta,tool_call,tool_result,delta,usage" {
		t.Fatalf("chunks = %s", k)
	}
	if tc := chunks[2].ToolCall; tc.Kind != "execute" || tc.Title != "ls" || !tc.Native {
		t.Fatalf("tool call = %+v", tc)
	}
	if r := chunks[3].ToolResult; r.Output != "a.txt" || r.Name != "Bash" {
		t.Fatalf("tool result = %+v", r)
	}
	if u := chunks[5].Usage; u.InputTokens != 10 || u.OutputTokens != 4 {
		t.Fatalf("usage = %+v", u)
	}
	if nt.SessionID != "T-1" {
		t.Fatalf("session = %q", nt.SessionID)
	}
	args := f.args(0)
	for _, want := range []string{"--execute", "--stream-json-thinking", "--stream-json-input", "--dangerously-allow-all", "--mode medium"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args = %s, missing %s", args, want)
		}
	}

	// The same process serves the next turn; a launch argument changing restarts it on the same thread.
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAutonomous, "T-1", "again")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("launches = %v", f.launches)
	}
	p.effort, p.fast = "high", true
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAutonomous, "T-1", "third")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "threads continue T-1 --execute") || !strings.Contains(f.args(1), "--effort high --fast") {
		t.Fatalf("launches = %v", f.launches)
	}
}

// Amp can't be sandboxed or asked, so any mode but Run is refused rather than quietly running unconfined.
func TestAmpRefusesModesItCannotHonour(t *testing.T) {
	f := &liveFake{}
	hang := false
	p := &CLISubprocessProvider{Kind: CLIKindAmp, Dir: t.TempDir(), Start: f.starter(ampScript(&hang))}
	for _, mode := range []agent.Mode{agent.ModeReadOnly, agent.ModeReadWrite, agent.ModeAcceptEdits, agent.ModeAuto, agent.ModePlan} {
		if _, err := runNative(t, p, liveTurn("", mode, "", "x")); err == nil || !strings.Contains(err.Error(), "full access only") {
			t.Fatalf("%s: err = %v", mode, err)
		}
	}
	if f.launchCount() != 0 {
		t.Fatal("a refused mode started the process")
	}
	specs := ampModes()
	if len(specs) != 1 || specs[0].Mode != "autonomous" {
		t.Fatalf("amp offers %+v", specs)
	}
}

// A message steered into the turn carries "steer": true; without it Amp would run it as a turn of its own.
func TestAmpSteerIsMarked(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	prompts := 0
	p := &CLISubprocessProvider{Kind: CLIKindAmp, Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		prompts++
		line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "T-1"})
		if prompts == 2 { // the steer is what lets the turn finish
			line(out, map[string]any{"type": "assistant", "message": map[string]any{"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		}
	})}
	nt := liveTurn(key, agent.ModeAutonomous, "", "first")
	steered := make(chan error, 1)
	steerAfterPrompt(nt, func() bool { return len(f.prompts()) > 0 }, "also this", steered)
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	if err := <-steered; err != nil {
		t.Fatal(err)
	}
	users := f.wrote(func(m map[string]any) bool { return m["type"] == "user" })
	if len(users) != 2 || users[0]["steer"] != nil || users[1]["steer"] != true {
		t.Fatalf("user lines = %v", users)
	}
}

// Stopping ends the process (there is no interrupt); the thread is picked up again by the next turn.
func TestAmpStopEndsTheProcessAndTheThreadContinues(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := true
	p := &CLISubprocessProvider{Kind: CLIKindAmp, Dir: t.TempDir(), Start: f.starter(ampScript(&hang))}
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, liveTurn(key, agent.ModeAutonomous, "", "stop me")), nil, nil, "")
	go func() {
		for len(f.prompts()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	for range chunks {
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v", err)
	}
	f.waitClosed(t, 0)
	hang = false
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAutonomous, "T-1", "again")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "threads continue T-1") {
		t.Fatalf("launches = %v", f.launches)
	}
}

func TestAmpModelsAreItsModes(t *testing.T) {
	models, err := ListModels(ProviderConfig{Name: "amp", Kind: "cli_amp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
		if m.Capabilities["fastMode"] != true {
			t.Fatalf("%s has no fast mode", m.ID)
		}
		if specs := m.Capabilities["permissionModes"].([]ModeSpec); len(specs) != 1 || specs[0].Mode != "autonomous" {
			t.Fatalf("%s offers %+v", m.ID, specs)
		}
	}
	if strings.Join(ids, ",") != "low,medium,high,ultra" {
		t.Fatalf("models = %v", ids)
	}
}

// piScript is a Pi (or, with omp, an Oh My Pi) RPC agent. While hang is set a prompt
// starts but doesn't settle until it is steered or aborted.
func piScript(omp bool, hang *bool, refuse *bool) func(int, map[string]any, io.Writer) {
	settle := func(out io.Writer) {
		if omp {
			line(out, map[string]any{"type": "agent_end", "isTerminal": false}) // an intermediate settle
			line(out, map[string]any{"type": "agent_end", "isTerminal": true})
			return
		}
		line(out, map[string]any{"type": "agent_settled"})
	}
	ok := func(out io.Writer, m map[string]any, data any) {
		line(out, map[string]any{"type": "response", "id": m["id"], "success": true, "data": data})
	}
	return func(_ int, m map[string]any, out io.Writer) {
		switch m["type"] {
		case "get_state":
			ok(out, m, map[string]any{"sessionId": "sid-1", "sessionFile": "/sessions/one.jsonl"})
		case "switch_session", "set_model", "set_thinking_level":
			ok(out, m, map[string]any{})
		case "prompt":
			if *refuse {
				line(out, map[string]any{"type": "response", "id": m["id"], "success": false, "error": "no API key for provider"})
				return
			}
			ok(out, m, map[string]any{})
			line(out, map[string]any{"type": "agent_start"})
			line(out, map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": "confirm"})
			line(out, map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant"}})
			if *hang {
				return
			}
			line(out, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "hm"}})
			line(out, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "Looking. "}})
			line(out, map[string]any{"type": "tool_execution_start", "toolCallId": "tc1", "toolName": "bash", "args": map[string]any{"command": "ls"}})
			line(out, map[string]any{"type": "tool_execution_end", "toolCallId": "tc1", "toolName": "bash", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "a.txt"}}}})
			// A message that streamed nothing still has its content at message_end.
			line(out, map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant"}})
			line(out, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Found a.txt."}}}})
			settle(out)
		case "steer", "abort":
			ok(out, m, map[string]any{})
			if *hang {
				*hang = false
				settle(out)
			}
		}
	}
}

func rpcTypes(f *liveFake) []string {
	var out []string
	for _, m := range f.wrote(func(map[string]any) bool { return true }) {
		out = append(out, m["type"].(string))
	}
	return out
}

func TestPiTurn(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang, refuse := false, false
	p := &CLISubprocessProvider{Kind: CLIKindPi, Model: "anthropic/claude-x", effort: "high", Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}

	nt := liveTurn(key, agent.ModeAutonomous, "", "list files")
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(chunks); k != "reasoning,delta,tool_call,tool_result,delta" {
		t.Fatalf("chunks = %s", k)
	}
	if r := chunks[3].ToolResult; r.Output != "a.txt" || r.Name != "bash" {
		t.Fatalf("tool result = %+v", r)
	}
	if chunks[4].Delta != "Found a.txt." {
		t.Fatalf("the message that streamed nothing lost its text: %+v", chunks[4])
	}
	// Resuming needs the session's file, so that is the session id the chat keeps.
	if nt.SessionID != "/sessions/one.jsonl" {
		t.Fatalf("session = %q", nt.SessionID)
	}
	// Pi needs its version check skipped, which goes through env(1); the handshake is in this order.
	if got := f.args(0); got != "env PI_SKIP_VERSION_CHECK=1 pi --mode rpc --approve" {
		t.Fatalf("launch = %s", got)
	}
	if got := strings.Join(rpcTypes(f), ","); !strings.HasPrefix(got, "get_state,set_model,set_thinking_level,get_state,prompt") {
		t.Fatalf("requests = %s", got)
	}
	// The extension's question got no answer from a person: it was cancelled.
	var ui []map[string]any
	for deadline := time.Now().Add(2 * time.Second); len(ui) == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		ui = f.wrote(func(m map[string]any) bool { return m["type"] == "extension_ui_response" })
	}
	if len(ui) != 1 || ui[0]["id"] != "ui-1" || ui[0]["cancelled"] != true {
		t.Fatalf("extension response = %v", ui)
	}

	// The same process serves the next turn; a new model or level is set on it, not a restart.
	p.Model, p.effort = "openai/gpt-x", "low"
	if _, err := runNative(t, p, liveTurn(key, agent.ModeAutonomous, "/sessions/one.jsonl", "again")); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("launches = %v", f.launches)
	}
	sm := f.wrote(func(m map[string]any) bool { return m["type"] == "set_model" })
	if len(sm) != 2 || sm[1]["provider"] != "openai" || sm[1]["modelId"] != "gpt-x" {
		t.Fatalf("set_model = %v", sm)
	}
}

func TestPiModelsMustBeProviderSlashModel(t *testing.T) {
	f := &liveFake{}
	hang, refuse := false, false
	p := &CLISubprocessProvider{Kind: CLIKindPi, Model: "gpt-x", Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}
	if _, err := runNative(t, p, liveTurn("", agent.ModeAutonomous, "", "x")); err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatalf("err = %v", err)
	}
}

func TestPiRefusesModesItCannotHonour(t *testing.T) {
	f := &liveFake{}
	hang, refuse := false, false
	for _, kind := range []CLIKind{CLIKindPi, CLIKindOhMyPi} {
		p := &CLISubprocessProvider{Kind: kind, Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}
		for _, mode := range []agent.Mode{agent.ModeReadOnly, agent.ModeReadWrite, agent.ModeAcceptEdits, agent.ModeAuto, agent.ModePlan} {
			if _, err := runNative(t, p, liveTurn("", mode, "", "x")); err == nil || !strings.Contains(err.Error(), "full access only") {
				t.Fatalf("%s/%s: err = %v", kind, mode, err)
			}
		}
	}
	if f.launchCount() != 0 {
		t.Fatal("a refused mode started the process")
	}
}

func TestOhMyPiOnlySettlesOnATerminalAgentEnd(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang, refuse := false, false
	p := &CLISubprocessProvider{Kind: CLIKindOhMyPi, Dir: t.TempDir(), Start: f.starter(piScript(true, &hang, &refuse))}
	chunks, err := runNative(t, p, liveTurn(key, agent.ModeAutonomous, "", "go"))
	if err != nil {
		t.Fatal(err)
	}
	// The intermediate agent_end came before the last message; ending there would have lost it.
	if last := chunks[len(chunks)-1]; last.Delta != "Found a.txt." {
		t.Fatalf("the turn ended early: %s", kinds(chunks))
	}
	if got := f.args(0); got != "omp --mode rpc --yolo" {
		t.Fatalf("launch = %s", got)
	}
}

func TestPiPromptRefusedFailsTheTurn(t *testing.T) {
	f := &liveFake{}
	hang, refuse := false, true
	p := &CLISubprocessProvider{Kind: CLIKindPi, Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}
	if _, err := runNative(t, p, liveTurn("", agent.ModeAutonomous, "", "go")); err == nil || !strings.Contains(err.Error(), "no API key for provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestPiResumeSwitchesToTheSessionFile(t *testing.T) {
	f := &liveFake{}
	hang, refuse := false, false
	p := &CLISubprocessProvider{Kind: CLIKindPi, Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}
	if _, err := runNative(t, p, liveTurn("", agent.ModeAutonomous, "/sessions/old.jsonl", "go")); err != nil {
		t.Fatal(err)
	}
	sw := f.wrote(func(m map[string]any) bool { return m["type"] == "switch_session" })
	if len(sw) != 1 || sw[0]["sessionPath"] != "/sessions/old.jsonl" {
		t.Fatalf("switch_session = %v", sw)
	}
}

func TestPiSteerAndAbort(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang, refuse := true, false
	p := &CLISubprocessProvider{Kind: CLIKindPi, Dir: t.TempDir(), Start: f.starter(piScript(false, &hang, &refuse))}

	// A steer is a request of its own; here it is what lets the turn settle.
	nt := liveTurn(key, agent.ModeAutonomous, "", "first")
	steered := make(chan error, 1)
	steerAfterPrompt(nt, func() bool { return len(f.wrote(func(m map[string]any) bool { return m["type"] == "prompt" })) > 0 }, "also this", steered)
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	if err := <-steered; err != nil {
		t.Fatal(err)
	}
	st := f.wrote(func(m map[string]any) bool { return m["type"] == "steer" })
	if len(st) != 1 || st[0]["message"] != "also this" {
		t.Fatalf("steer = %v", st)
	}

	// Stopping aborts the turn and keeps the process.
	hang = true
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, liveTurn(key, agent.ModeAutonomous, "/sessions/one.jsonl", "stop me")), nil, nil, "")
	go func() {
		for len(f.wrote(func(m map[string]any) bool { return m["type"] == "prompt" })) < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	for range chunks {
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.wrote(func(m map[string]any) bool { return m["type"] == "abort" })); n != 1 || f.launchCount() != 1 {
		t.Fatalf("aborts %d, launches %d", n, f.launchCount())
	}
}

func TestDiscoverPiModels(t *testing.T) {
	respond := func(models []any) func(int, map[string]any, io.Writer) {
		return func(_ int, m map[string]any, out io.Writer) {
			if m["type"] == "get_available_models" {
				line(out, map[string]any{"type": "response", "id": m["id"], "success": true, "data": map[string]any{"models": models}})
			}
		}
	}
	// Pi maps levels per model, and null means unavailable; xhigh and max must be mapped to count.
	f := &liveFake{}
	useDiscovery(t, f, respond([]any{
		map[string]any{"provider": "anthropic", "id": "claude-x", "name": "Claude X", "reasoning": true,
			"thinkingLevelMap": map[string]any{"minimal": nil, "xhigh": "high"}},
		map[string]any{"provider": "openai", "id": "plain", "reasoning": false},
		map[string]any{"provider": "", "id": "orphan"},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := discoverPiModels(ctx, piFlavorPi, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "anthropic/claude-x" || got[0].DisplayName != "Claude X" || got[1].ID != "openai/plain" {
		t.Fatalf("models = %+v", got)
	}
	if e := got[0].Capabilities["reasoningEfforts"]; !reflect.DeepEqual(e, []string{"off", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("pi efforts = %v", e)
	}
	if got[1].Capabilities != nil {
		t.Fatalf("a model without reasoning has efforts: %v", got[1].Capabilities)
	}
	if !strings.Contains(f.args(0), "env PI_SKIP_VERSION_CHECK=1 pi --mode rpc --no-session --no-skills --no-prompt-templates --no-context-files") {
		t.Fatalf("probe = %s", f.args(0))
	}

	// Oh My Pi lists the levels a model honours, and always accepts off.
	f2 := &liveFake{}
	useDiscovery(t, f2, respond([]any{
		map[string]any{"provider": "anthropic", "id": "claude-x", "reasoning": true, "thinking": map[string]any{"efforts": []any{"low", "high"}}},
	}))
	got, err = discoverPiModels(ctx, piFlavorOhMyPi, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if e := got[0].Capabilities["reasoningEfforts"]; !reflect.DeepEqual(e, []string{"off", "low", "high"}) {
		t.Fatalf("omp efforts = %v", e)
	}
	if !strings.Contains(f2.args(0), "omp --mode rpc --no-session --no-skills --no-rules --no-extensions") {
		t.Fatalf("probe = %s", f2.args(0))
	}
}
