package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// fakeCLI wires a provider to an in-process stand-in for the CLI: script
// reads what the provider writes to stdin and writes the CLI's stdout.
// The returned wait blocks until the script has finished.
func fakeCLI(t *testing.T, gotArgs *[]string, script func(in *bufio.Scanner, out io.Writer)) (procStarter, func()) {
	t.Helper()
	finished := make(chan struct{})
	return func(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
		*gotArgs = append([]string{name}, args...)
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(finished)
			defer close(done)
			defer outW.Close()
			sc := bufio.NewScanner(inR)
			sc.Buffer(make([]byte, 64*1024), 10<<20)
			script(sc, outW)
		}()
		go func() {
			<-ctx.Done()
			inR.CloseWithError(ctx.Err())
			outW.CloseWithError(ctx.Err())
		}()
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64*1024), 10<<20)
		return &nativeProc{stdin: inW, lines: sc, stderr: func() string { return "" }, wait: func() error { <-done; return nil }}, nil
	}, func() { <-finished }
}

func line(out io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = out.Write(append(b, '\n'))
}

// rpcServe answers each JSON-RPC request with handle's result (nil result =
// no reply yet — the handler writes it itself), and returns every message
// the provider sent, including responses to the fake's own requests.
func rpcServe(in *bufio.Scanner, out io.Writer, handle func(msg map[string]any) any) []map[string]any {
	var seen []map[string]any
	for in.Scan() {
		var msg map[string]any
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		seen = append(seen, msg)
		res := handle(msg)
		if _, isReq := msg["method"]; isReq && msg["id"] != nil && res != nil {
			line(out, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": res})
		}
	}
	return seen
}

type turnRecorder struct {
	mu     sync.Mutex
	asks   []agent.NativeAsk
	answer agent.NativeAnswer
}

func (r *turnRecorder) turn(mode agent.Mode) *agent.NativeTurn {
	return &agent.NativeTurn{
		Mode: mode, Prompt: "do it", FreshPrompt: "transcript + do it",
		Ask: func(_ context.Context, a agent.NativeAsk) (agent.NativeAnswer, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.asks = append(r.asks, a)
			return r.answer, nil
		},
		Questions: func(_ context.Context, qs []agent.NativeQuestion) ([]string, error) {
			out := make([]string, len(qs))
			for i := range out {
				out[i] = "blue"
			}
			return out, nil
		},
	}
}

func runNative(t *testing.T, p *CLISubprocessProvider, nt *agent.NativeTurn) ([]agent.ModelStreamChunk, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
	var got []agent.ModelStreamChunk
	for c := range chunks {
		got = append(got, c)
	}
	return got, <-errs
}

func kinds(chunks []agent.ModelStreamChunk) string {
	var k []string
	for _, c := range chunks {
		k = append(k, string(c.Kind))
	}
	return strings.Join(k, ",")
}

func TestClaudeNativeTurn(t *testing.T) {
	var args []string
	var control map[string]any
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true, Remember: true}}
	var limits *protocol.ProviderRateLimits
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir(),
		OnRateLimits: func(rl protocol.ProviderRateLimits) { limits = &rl },
	}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		in.Scan() // the prompt
		line(out, map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})
		line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Running ls."}}})
		line(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}})
		line(out, map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu1", "permission_suggestions": []any{map[string]any{"type": "addRules"}}}})
		in.Scan()
		_ = json.Unmarshal(in.Bytes(), &control)
		line(out, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "a.txt"}}}})
		line(out, map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.47, "resetsAt": 1790209200}}}})
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "done", "total_cost_usd": 0.01, "usage": map[string]any{"input_tokens": 2, "cache_read_input_tokens": 100, "output_tokens": 5}})
	})
	nt := rec.turn(agent.ModeReadWrite)
	nt.ResumeID = "sess-0"
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	wait()
	if got := strings.Join(args, " "); !strings.Contains(got, "--permission-prompt-tool stdio") || !strings.Contains(got, "--permission-mode default") || !strings.Contains(got, "--resume sess-0") || !strings.Contains(got, "--model haiku") {
		t.Fatalf("args = %s", got)
	}
	if strings.Contains(strings.Join(args, " "), "--settings") {
		t.Fatalf("read_write turn got autonomous sandbox settings: %v", args)
	}
	if k := kinds(chunks); k != "delta,tool_call,tool_result,usage" {
		t.Fatalf("chunks = %s", k)
	}
	tc := chunks[1].ToolCall
	if !tc.Native || tc.Kind != "execute" || tc.Title != "ls" {
		t.Fatalf("tool call = %+v", tc)
	}
	if chunks[2].ToolResult.Output != "a.txt" || chunks[2].ToolResult.Name != "Bash" {
		t.Fatalf("result = %+v", chunks[2].ToolResult)
	}
	if u := chunks[3].Usage; u.InputTokens != 102 || u.CostUSD != 0.01 || !u.CostKnown {
		t.Fatalf("usage = %+v", u)
	}
	if len(rec.asks) != 1 || rec.asks[0].ToolCallID != "tu1" {
		t.Fatalf("asks = %+v", rec.asks)
	}
	resp, _ := control["response"].(map[string]any)
	body, _ := resp["response"].(map[string]any)
	if resp["request_id"] != "r1" || body["behavior"] != "allow" || body["updatedPermissions"] == nil {
		t.Fatalf("control response = %v", control)
	}
	if nt.SessionID != "sess-1" || limits == nil || limits.FiveHour.UsedPct != 47 {
		t.Fatalf("session %q, limits %+v", nt.SessionID, limits)
	}
}

func TestCodexNativeTurn(t *testing.T) {
	var args []string
	var seen []map[string]any
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Dir: t.TempDir(), effort: "high"}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		seen = rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{}
			case "thread/resume":
				line(out, map[string]any{"id": msg["id"], "error": map[string]any{"code": -32000, "message": "no rollout"}})
				return nil
			case "thread/start":
				return map[string]any{"thread": map[string]any{"id": "th1"}}
			case "turn/start":
				line(out, map[string]any{"id": msg["id"], "result": map[string]any{"turn": map[string]any{"id": "t1"}}})
				line(out, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "hi"}})
				line(out, map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]any{"type": "fileChange", "id": "fc1", "status": "inProgress", "changes": []any{map[string]any{"path": "a.txt", "kind": map[string]any{"type": "update"}, "diff": "@@ -1,1 +1,1 @@\n-old\n+new\n"}}}}})
				line(out, map[string]any{"id": 900, "method": "item/fileChange/requestApproval", "params": map[string]any{"itemId": "fc1"}})
				return nil
			case nil: // our answer to request 900
				line(out, map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{"type": "fileChange", "id": "fc1", "status": "completed", "changes": []any{map[string]any{"path": "a.txt", "diff": "@@ -1,1 +1,1 @@\n-old\n+new\n"}}}}})
				line(out, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 10, "outputTokens": 3}}}})
				line(out, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"status": "completed"}}})
				return nil
			case "account/rateLimits/read":
				return map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 12, "windowDurationMins": 300, "resetsAt": 1790209200}}}
			}
			return map[string]any{}
		})
	})
	nt := rec.turn(agent.ModeReadWrite)
	nt.ResumeID = "gone"
	var limits *protocol.ProviderRateLimits
	p.OnRateLimits = func(rl protocol.ProviderRateLimits) { limits = &rl }
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	wait()
	if k := kinds(chunks); k != "delta,tool_call,tool_result,usage" {
		t.Fatalf("chunks = %s", k)
	}
	if r := chunks[2].ToolResult; len(r.Diffs) != 1 || len(r.Diffs[0].Hunks) != 1 || len(r.Diffs[0].Hunks[0].Lines) != 2 {
		t.Fatalf("result diffs = %+v", r)
	}
	if len(rec.asks) != 1 || rec.asks[0].ToolCallID != "fc1" || rec.asks[0].Diff == nil {
		t.Fatalf("asks = %+v", rec.asks)
	}
	if nt.SessionID != "th1" || chunks[3].Usage.InputTokens != 10 || limits == nil || limits.FiveHour == nil {
		t.Fatalf("session %q usage %+v limits %+v", nt.SessionID, chunks[3].Usage, limits)
	}
	var decision, turnStart map[string]any
	for _, m := range seen {
		if m["id"] == float64(900) {
			decision, _ = m["result"].(map[string]any)
		}
		if m["method"] == "turn/start" {
			turnStart, _ = m["params"].(map[string]any)
		}
	}
	if decision["decision"] != "accept" {
		t.Fatalf("approval answer = %v", decision)
	}
	// A failed resume falls back to the transcript, not the bare message.
	input := turnStart["input"].([]any)[0].(map[string]any)
	if input["text"] != "transcript + do it" || turnStart["effort"] != "high" {
		t.Fatalf("turn/start = %v", turnStart)
	}
}

func TestACPNativeTurn(t *testing.T) {
	var args []string
	var seen []map[string]any
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true, Remember: true}}
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Model: "k2", Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		update := func(u map[string]any) {
			line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
		}
		seen = rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}}
			case "session/load":
				// Replayed history must not reach the turn.
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "OLD"}})
				update(map[string]any{"sessionUpdate": "usage_update", "used": 1, "size": 2, "cost": map[string]any{"amount": 1.0, "currency": "USD"}})
				return map[string]any{"configOptions": []any{map[string]any{"id": "model", "category": "model", "options": []any{}}}}
			case "session/set_config_option":
				return map[string]any{}
			case "session/prompt":
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "editing"}})
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "Edit a.txt", "kind": "edit", "status": "pending",
					"content": []any{map[string]any{"type": "diff", "path": "a.txt", "oldText": "x\n", "newText": "y\n"}}})
				line(out, map[string]any{"jsonrpc": "2.0", "id": 77, "method": "session/request_permission", "params": map[string]any{"sessionId": "s1",
					"toolCall": map[string]any{"toolCallId": "c1"},
					"options":  []any{map[string]any{"optionId": "once", "kind": "allow_once"}, map[string]any{"optionId": "always", "kind": "allow_always"}}}})
				go func(id any) {
					time.Sleep(50 * time.Millisecond)
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed"})
					update(map[string]any{"sessionUpdate": "usage_update", "used": 1, "size": 2, "cost": map[string]any{"amount": 1.25, "currency": "USD"}})
					line(out, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
				}(msg["id"])
				return nil
			}
			return nil
		})
	})
	nt := rec.turn(agent.ModeReadWrite)
	nt.ResumeID, nt.SessionCostUSD = "s1", 0.5
	chunks, err := runNative(t, p, nt)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	wait()
	if args[0] != "kimi" || args[1] != "acp" {
		t.Fatalf("args = %v", args)
	}
	if k := kinds(chunks); k != "delta,tool_call,tool_result,usage" || chunks[0].Delta != "editing" {
		t.Fatalf("chunks = %s (%q)", k, chunks[0].Delta)
	}
	if r := chunks[2].ToolResult; len(r.Diffs) != 1 || r.Diffs[0].FilePath != "a.txt" {
		t.Fatalf("result = %+v", r)
	}
	if u := chunks[3].Usage; !u.CostKnown || u.CostUSD != 0.25 || nt.SessionCostUSD != 1.25 {
		t.Fatalf("usage %+v, session cost %v (baseline comes from the load replay)", u, nt.SessionCostUSD)
	}
	var picked, setModel map[string]any
	for _, m := range seen {
		if m["id"] == float64(77) {
			picked, _ = m["result"].(map[string]any)
		}
		if m["method"] == "session/set_config_option" {
			setModel, _ = m["params"].(map[string]any)
		}
	}
	if o, _ := picked["outcome"].(map[string]any); o["optionId"] != "always" {
		t.Fatalf("permission answer = %v", picked)
	}
	if setModel["value"] != "k2" || len(rec.asks) != 1 || rec.asks[0].Diff == nil || nt.SessionID != "s1" {
		t.Fatalf("set model %v, asks %+v, session %q", setModel, rec.asks, nt.SessionID)
	}
}

func TestParseCodexRateLimits(t *testing.T) {
	rl, err := parseCodexRateLimits([]byte(`{"primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1790209200},"secondary":{"usedPercent":100,"windowDurationMins":43200,"resetsAt":1790790203}}`))
	if err != nil || rl.FiveHour == nil || rl.FiveHour.UsedPct != 12 || rl.SevenDay != nil || len(rl.Other) != 1 || rl.Other[0].WindowMins != 43200 {
		t.Fatalf("unexpected: %+v, %v", rl, err)
	}
}

func TestDetectCLI(t *testing.T) {
	dir := t.TempDir()
	for _, bin := range []string{"claude", "cursor-agent"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if !DetectCLI(CLIKindClaude) || !DetectCLI(CLIKindCursor) || DetectCLI(CLIKindCodex) || DetectCLI(CLIKindKimi) {
		t.Fatal("DetectCLI should find exactly the binaries on PATH")
	}
}

// Autonomous bypasses Claude's prompts, so its own sandbox must be on (and
// refuse to fall back to running unsandboxed).
func TestClaudeAutonomousTurnIsSandboxed(t *testing.T) {
	var args []string
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		in.Scan()
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "done"})
	})
	rec := &turnRecorder{}
	if _, err := runNative(t, p, rec.turn(agent.ModeAutonomous)); err != nil {
		t.Fatalf("turn: %v", err)
	}
	wait()
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--permission-mode bypassPermissions") || !strings.Contains(got, "--settings "+claudeAutonomousSandbox) {
		t.Fatalf("args = %s", got)
	}
	var cfg struct {
		Sandbox struct{ Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool }
	}
	if err := json.Unmarshal([]byte(claudeAutonomousSandbox), &cfg); err != nil || !cfg.Sandbox.Enabled || !cfg.Sandbox.FailIfUnavailable || cfg.Sandbox.AllowUnsandboxedCommands {
		t.Fatalf("sandbox settings = %+v, %v", cfg, err)
	}
}
