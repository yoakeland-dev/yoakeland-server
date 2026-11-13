package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

// cliPlan is what the fake Claude Code does with one prompt: rounds of tool calls (ids per
// assistant message, each really sent to the MCP endpoint as the CLI would), then an answer
// that contains what the calls returned.
type cliPlan struct {
	rounds   [][]string
	parallel bool   // the calls go out together, not one after the other
	answer   string // default "answer:"
	stall    bool   // the first message's block starts and its call goes out, but the message never ends
	noCall   bool   // the calls are never sent: the test sends them
	exit     string // "result": leaves before the result; "after": leaves after it
	badInit  string // "failed" or "notools"
	bash     bool   // asks permission for a tool that was never offered
}

type mcpEnd struct{ url, auth string }

func (f *liveFake) endpoint(t *testing.T, launch int) mcpEnd {
	t.Helper()
	f.mu.Lock()
	args := f.launches[launch]
	f.mu.Unlock()
	for i, a := range args {
		if a == "--mcp-config" {
			raw, err := os.ReadFile(args[i+1])
			if err != nil {
				t.Fatalf("mcp config: %v", err)
			}
			var cfg struct {
				MCPServers map[string]struct {
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			y := cfg.MCPServers["y"]
			return mcpEnd{y.URL, y.Headers["Authorization"]}
		}
	}
	t.Fatal("no --mcp-config")
	return mcpEnd{}
}

// rpc posts one JSON-RPC request the way the CLI does; rpcErr is the error message of an MCP error.
func (e mcpEnd) rpc(ctx context.Context, method string, params any) (result map[string]any, rpcErr string, err error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	req.Header.Set("Authorization", e.auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	if out.Error != nil {
		return nil, out.Error.Message, nil
	}
	return out.Result, "", nil
}

func (e mcpEnd) call(ctx context.Context, id string) (string, string, error) {
	res, rerr, err := e.rpc(ctx, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"n": id}, "_meta": map[string]any{"claudecode/toolUseId": id}})
	if err != nil || rerr != "" {
		return "", rerr, err
	}
	c, _ := res["content"].([]any)
	first, _ := c[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, "", nil
}

func (f *liveFake) cli(t *testing.T, plan func(prompt string) cliPlan) func(int, map[string]any, io.Writer) {
	var mu sync.Mutex
	turns := map[int]int{}
	return func(launch int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		msg, _ := m["message"].(map[string]any)
		c, _ := msg["content"].([]any)
		b, _ := c[0].(map[string]any)
		prompt, _ := b["text"].(string)
		mu.Lock()
		turns[launch]++
		n := turns[launch]
		mu.Unlock()
		p := plan(prompt)
		go func() {
			init := map[string]any{"type": "system", "subtype": "init", "tools": []string{"mcp__y__echo"},
				"mcp_servers": []map[string]string{{"name": "y", "status": "connected"}}}
			switch p.badInit {
			case "failed":
				init["mcp_servers"] = []map[string]string{{"name": "y", "status": "failed"}}
			case "notools":
				init["tools"] = []string{}
			case "builtin":
				init["tools"] = []string{"mcp__y__echo", "Bash"}
			}
			line(out, init)
			if p.bash {
				line(out, map[string]any{"type": "control_request", "request_id": "perm1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}}})
				return
			}
			line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "thinking_delta", "thinking": "hmm"}}})
			var texts []string
			for ri, ids := range p.rounds {
				line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "let me look. "}}})
				for _, id := range ids {
					line(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "mcp__y__echo", "input": map[string]any{"n": id}}},
						"usage": map[string]any{"input_tokens": 2, "cache_read_input_tokens": 10, "output_tokens": 1}}})
				}
				e := f.endpoint(t, launch)
				if p.stall && ri == 0 {
					for _, id := range ids {
						go e.call(context.Background(), id)
					}
					return
				}
				stop := func() {
					line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "usage": map[string]any{"input_tokens": 2, "cache_read_input_tokens": 10, "output_tokens": 5}}})
					line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
				}
				stop()
				if p.noCall {
					return
				}
				// The calls are out as the message ends; their results only come with the next round.
				res := make([]string, len(ids))
				var wg sync.WaitGroup
				for i, id := range ids {
					run := func() { defer wg.Done(); res[i], _, _ = e.call(context.Background(), id) }
					wg.Add(1)
					if p.parallel {
						go run()
					} else {
						run()
					}
				}
				wg.Wait()
				texts = append(texts, res...)
			}
			if p.exit == "result" {
				out.(io.Closer).Close()
				return
			}
			ans := cmpOr(p.answer, "answer:") + strings.Join(texts, "|")
			line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": ans}}})
			line(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": ans}}}})
			line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "usage": map[string]any{"input_tokens": 3, "cache_creation_input_tokens": 4, "cache_read_input_tokens": 20, "output_tokens": 7}}})
			line(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
			line(out, map[string]any{"type": "result", "subtype": "success", "result": ans, "total_cost_usd": 0.01 * float64(n),
				"modelUsage": map[string]any{"m": map[string]any{"contextWindow": 200000}},
				"usage":      map[string]any{"input_tokens": 99, "output_tokens": 99}})
			if p.exit == "after" {
				out.(io.Closer).Close()
			}
		}()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- harness ----

type cmHarness struct {
	t    *testing.T
	f    *liveFake
	m    *ClaudeModel
	key  string
	dirs []string
	env  []string
}

var echoTool = []tool.ToolDefinition{{Name: "echo", Description: "Echo.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "string"}}}}}

func newHarness(t *testing.T, plan func(prompt string) cliPlan) *cmHarness {
	h := &cmHarness{t: t, f: &liveFake{}, key: t.Name()}
	script := h.f.cli(t, plan)
	base := h.f.starter(script)
	h.m = &ClaudeModel{Model: "haiku", ConfigDir: "/acct", SessionKey: h.key, Start: func(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
		h.env = procEnvFrom(ctx)
		h.dirs = append(h.dirs, dir)
		if strings.Contains(strings.Join(args, " "), "--output-format json") { // a one-shot
			return claudeStub(&[]string{}, &strings.Builder{}, `{"result":"A Title","total_cost_usd":0.002,"usage":{"input_tokens":2,"output_tokens":5}}`).Start(ctx, name, args, dir)
		}
		return base(ctx, name, args, dir)
	}}
	t.Cleanup(func() {
		claudeModels.mu.Lock()
		s := claudeModels.live[h.key]
		claudeModels.mu.Unlock()
		if s != nil {
			s.close()
		}
	})
	return h
}

func (h *cmHarness) session() *modelSession {
	claudeModels.mu.Lock()
	defer claudeModels.mu.Unlock()
	return claudeModels.live[h.key]
}

type roundOut struct {
	text, reasoning string
	calls           []agent.ToolCall
	usage           []*agentUsage
	err             error
	all             []agent.ModelStreamChunk
}
type agentUsage = struct {
	in, out, ctx int
	cost         float64
	known        bool
}

func (h *cmHarness) run(ctx context.Context, hist []agent.Message) roundOut {
	h.t.Helper()
	return h.runWith(ctx, hist, echoTool, "sys")
}

func (h *cmHarness) runWith(ctx context.Context, hist []agent.Message, tools []tool.ToolDefinition, sys string) roundOut {
	h.t.Helper()
	chunks, errs := h.m.StreamTurn(ctx, hist, tools, sys)
	var r roundOut
	timeout := time.After(10 * time.Second)
	for chunks != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				break
			}
			r.all = append(r.all, c)
			switch c.Kind {
			case agent.StreamChunkDelta:
				r.text += c.Delta
			case agent.StreamChunkReasoning:
				r.reasoning += c.Delta
			case agent.StreamChunkToolCall:
				r.calls = append(r.calls, *c.ToolCall)
			case agent.StreamChunkUsage:
				u := c.Usage
				r.usage = append(r.usage, &agentUsage{u.InputTokens, u.OutputTokens, u.ContextTokens, u.CostUSD, u.CostKnown})
			}
		case <-timeout:
			h.t.Fatal("round didn't finish")
		}
	}
	r.err = <-errs
	return r
}

// next is the history the agent sends after a round: its own rendering of what streamed, as
// flushSegment stores it (text, reasoning, the calls with ids, provider and model), then results.
func next(h []agent.Message, r roundOut, results ...agent.Message) []agent.Message {
	var tcs []agent.ToolCall
	for _, c := range r.calls {
		tcs = append(tcs, agent.ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
	}
	out := append(append([]agent.Message(nil), h...), agent.Message{ID: fmt.Sprintf("m%d", len(h)), Role: "assistant", Content: r.text, Reasoning: r.reasoning, ToolCalls: tcs, Provider: "lent", Model: "haiku"})
	return append(out, results...)
}

func result(id, text string) agent.Message {
	return agent.Message{ID: "t" + id, Role: "tool", ToolCallID: id, Content: text}
}

func user(text string) agent.Message { return agent.Message{Role: "user", Content: text} }

func gone(t *testing.T, f *liveFake, launch int) { t.Helper(); f.waitClosed(t, launch) }

// ---- tests ----

func TestClaudeModelLaunchAndToolList(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}} })
	r := h.run(context.Background(), []agent.Message{user("hi")})
	if r.err != nil || len(r.calls) != 1 {
		t.Fatalf("%v %+v", r.err, r.calls)
	}
	args := h.f.args(0)
	sys := "sys" + claudeModelPrompt
	cfgArg := ""
	for _, a := range h.f.launches[0] {
		if strings.HasSuffix(a, ".json") {
			cfgArg = a
		}
	}
	want := `claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages --tools  --mcp-config ` + cfgArg +
		` --strict-mcp-config --setting-sources  --settings {"disableAllHooks":true} --disable-slash-commands --system-prompt ` + sys + ` --system-prompt-snapshot off --permission-prompt-tool stdio --allowedTools mcp__y --model haiku`
	if args != want {
		t.Fatalf("args:\n%s\nwant\n%s", args, want)
	}
	if !strings.Contains(sys, "mcp__y__") || !strings.Contains(strings.Join(h.env, " "), "MCP_TOOL_TIMEOUT=86400000") || !strings.Contains(strings.Join(h.env, " "), "CLAUDE_CONFIG_DIR=/acct") {
		t.Fatalf("env %v", h.env)
	}
	if entries, err := os.ReadDir(h.dirs[0]); err != nil || len(entries) != 0 {
		t.Fatalf("scratch dir %v %v", entries, err)
	}
	e := h.f.endpoint(t, 0)
	res, rerr, err := e.rpc(context.Background(), "tools/list", nil)
	tl, _ := res["tools"].([]any)
	if err != nil || rerr != "" || len(tl) != 1 {
		t.Fatalf("tools/list %v %v %v", res, rerr, err)
	}
	t0 := tl[0].(map[string]any)
	if t0["name"] != "echo" || t0["description"] != "Echo." || t0["inputSchema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool %v", t0)
	}
	if bad := (mcpEnd{e.url, "Bearer nope"}); true {
		if _, _, err := bad.rpc(context.Background(), "tools/list", nil); err == nil {
			t.Fatal("wrong token accepted")
		}
	}
	// Ending the session removes the scratch dir, the config file and the listener.
	h.session().close()
	if _, err := os.Stat(h.dirs[0]); err == nil {
		t.Fatal("scratch dir kept")
	}
	if _, err := os.Stat(cfgArg); err == nil {
		t.Fatal("mcp config kept")
	}
	if _, _, err := e.rpc(context.Background(), "tools/list", nil); err == nil {
		t.Fatal("listener kept")
	}
}

func TestClaudeModelTwoRoundsInOneProcess(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}} })
	h1 := []agent.Message{user("hi")}
	r1 := h.run(context.Background(), h1)
	if r1.err != nil || len(r1.calls) != 1 || r1.calls[0].Name != "echo" || r1.calls[0].ID != "tu1" || r1.calls[0].Native || r1.text != "let me look. " || r1.reasoning != "hmm" {
		t.Fatalf("round 1: %+v", r1)
	}
	// Usage for the round's own message, no cost yet but marked known so the borrower doesn't price it.
	if len(r1.usage) != 1 || *r1.usage[0] != (agentUsage{in: 12, out: 5, ctx: 17, known: true}) {
		t.Fatalf("round 1 usage %+v", r1.usage[0])
	}
	h2 := next(h1, r1, result("tu1", "RESULT"))
	r2 := h.run(context.Background(), h2)
	if r2.err != nil || r2.text != "answer:RESULT" || len(r2.calls) != 0 {
		t.Fatalf("round 2: %+v", r2)
	}
	if u := r2.usage[len(r2.usage)-1]; u.cost != 0.01 || !u.known || u.in != 27 || u.out != 7 {
		t.Fatalf("final usage %+v", u)
	}
	if h.f.launchCount() != 1 || len(h.f.prompts()) != 1 {
		t.Fatalf("launches %d prompts %v", h.f.launchCount(), h.f.prompts())
	}
	// Case 2: one new user message goes to the same process.
	h3 := append(next(h2, r2), user("and then?"))
	r3 := h.run(context.Background(), h3)
	if r3.err != nil || h.f.launchCount() != 1 || h.f.prompts()[1] != "and then?" {
		t.Fatalf("round 3: %+v launches %d prompts %v", r3, h.f.launchCount(), h.f.prompts())
	}
}

func TestClaudeModelTwoCallsAnsweredInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parallel bool
		order    []string
	}{{"serial, results in call order", false, []string{"a", "b"}}, {"parallel, results reversed", true, []string{"b", "a"}}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"a", "b"}}, parallel: tc.parallel} })
			h1 := []agent.Message{user("hi")}
			r1 := h.run(context.Background(), h1)
			if len(r1.calls) != 2 {
				t.Fatalf("%+v", r1)
			}
			var rs []agent.Message
			for _, id := range tc.order {
				rs = append(rs, result(id, "R-"+id))
			}
			r2 := h.run(context.Background(), next(h1, r1, rs...))
			if r2.err != nil || (r2.text != "answer:R-a|R-b" && r2.text != "answer:R-b|R-a") || h.f.launchCount() != 1 {
				t.Fatalf("%+v launches %d", r2, h.f.launchCount())
			}
		})
	}
}

func TestClaudeModelFreshSessionTriggers(t *testing.T) {
	base := func(h *cmHarness) (hist []agent.Message) {
		h1 := []agent.Message{user("one")}
		r1 := h.run(context.Background(), h1)
		if r1.err != nil {
			t.Fatal(r1.err)
		}
		return append(next(h1, r1), user("two"))
	}
	for _, tc := range []struct {
		name   string
		mutate func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string)
		want   string // in the second prompt
	}{
		{"edit", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			hist[0].Content = "one, edited"
			return hist, echoTool, "sys"
		}, "User: one, edited"},
		{"retry or branch", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			hist[2].Content = "two"
			return append(hist[:1:1], user("another")), echoTool, "sys"
		}, "another"},
		{"compaction", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			return []agent.Message{user("summary of before"), hist[2]}, echoTool, "sys"
		}, "summary of before"},
		{"another system prompt", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			return hist, echoTool, "other"
		}, "User: one"},
		{"another tool set", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			return hist, append(append([]tool.ToolDefinition(nil), echoTool...), tool.ToolDefinition{Name: "more"}), "sys"
		}, "User: one"},
		{"another model", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			h.m.Model = "opus"
			return hist, echoTool, "sys"
		}, "User: one"},
		{"another effort", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			h.m.SetModelOptions(agent.ModelOptions{ReasoningEffort: "high"})
			return hist, echoTool, "sys"
		}, "User: one"},
		{"a dead process", func(h *cmHarness, hist []agent.Message) ([]agent.Message, []tool.ToolDefinition, string) {
			h.session().proc.stdin.Close() // the fake leaves when its stdin ends
			<-h.session().exited
			return hist, echoTool, "sys"
		}, "User: one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(string) cliPlan { return cliPlan{} })
			hist := base(h)
			hist, tools, sys := tc.mutate(h, hist)
			r := h.runWith(context.Background(), hist, tools, sys)
			if r.err != nil || h.f.launchCount() != 2 {
				t.Fatalf("err %v launches %d", r.err, h.f.launchCount())
			}
			if p := h.f.prompts(); !strings.Contains(p[len(p)-1], tc.want) {
				t.Fatalf("prompt %q lacks %q", p[len(p)-1], tc.want)
			}
			gone(t, h.f, 0)
		})
	}
}

func TestClaudeModelFreshSessionAfterToolsCarriesWhatHappened(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{} })
	hist := []agent.Message{user("look"), {Role: "assistant", Content: "ok", ToolCalls: []agent.ToolCall{{ID: "x", Name: "echo", Args: json.RawMessage(`{}`)}}}, result("x", "FOUND")}
	if r := h.run(context.Background(), hist); r.err != nil {
		t.Fatal(r.err)
	}
	if p := h.f.prompts()[0]; !strings.HasPrefix(p, "look") || !strings.Contains(p, "[tool result] FOUND") {
		t.Fatalf("prompt %q", p)
	}
}

func TestClaudeModelCancelMidHold(t *testing.T) {
	first := true
	h := newHarness(t, func(string) cliPlan {
		st := first
		first = false
		return cliPlan{rounds: [][]string{{"tu1"}}, stall: st}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan roundOut, 1)
	go func() { done <- h.run(ctx, []agent.Message{user("hi")}) }()
	// The call is out and held before the message ends.
	for i := 0; i < 500 && h.f.launchCount() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 500 && func() bool { s := h.session(); s.mu.Lock(); defer s.mu.Unlock(); return s.calls["tu1"] == nil }(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	e := h.f.endpoint(t, 0)
	probe := make(chan string, 1)
	go func() { _, rerr, err := e.call(context.Background(), "tu1"); probe <- cmpOr(rerr, fmt.Sprint(err)) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err %v", r.err)
	}
	if len(h.f.controls("interrupt")) != 1 {
		t.Fatal("no interrupt sent")
	}
	select {
	case rerr := <-probe:
		if rerr == "" || rerr == "<nil>" {
			t.Fatal("the held call wasn't failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held call still held")
	}
	gone(t, h.f, 0)
	if h.session() != nil {
		t.Fatal("session kept")
	}
	// The next request starts over.
	r2 := h.run(context.Background(), []agent.Message{user("hi")})
	_ = r2
	if h.f.launchCount() != 2 {
		t.Fatalf("launches %d", h.f.launchCount())
	}
}

func TestClaudeModelProcessExitsMidRound(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{exit: "result"} })
	r := h.run(context.Background(), []agent.Message{user("hi")})
	if r.err == nil || !strings.Contains(r.err.Error(), "exited") {
		t.Fatalf("err %v", r.err)
	}
	if h.session() != nil {
		t.Fatal("dead session kept")
	}
}

func TestClaudeModelFailedToolServerIsFatal(t *testing.T) {
	for _, bad := range []string{"failed", "notools", "builtin"} {
		h := newHarness(t, func(string) cliPlan { return cliPlan{badInit: bad} })
		r := h.run(context.Background(), []agent.Message{user("hi")})
		if r.err == nil || !strings.Contains(r.err.Error(), "offered tools") {
			t.Fatalf("%s: err %v text %q", bad, r.err, r.text)
		}
		gone(t, h.f, 0)
	}
}

func TestClaudeModelDeniesToolsThatWereNotOffered(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{bash: true} })
	r := h.run(context.Background(), []agent.Message{user("hi")})
	if r.err == nil || !strings.Contains(r.err.Error(), "Bash") {
		t.Fatalf("err %v", r.err)
	}
	var denied bool
	for i := 0; i < 100 && !denied; i++ {
		for _, m := range h.f.wrote(func(m map[string]any) bool { return m["type"] == "control_response" }) {
			resp := m["response"].(map[string]any)
			inner, _ := resp["response"].(map[string]any)
			denied = denied || (resp["request_id"] == "perm1" && inner["behavior"] == "deny")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !denied {
		t.Fatal("no deny written")
	}
}

func TestClaudeModelToollessGoesToOneShot(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}} })
	h1 := []agent.Message{user("hi")}
	r1 := h.run(context.Background(), h1)
	s := h.session()
	title := h.runWith(context.Background(), []agent.Message{user("name this")}, nil, "title it")
	if title.err != nil || title.text != "A Title" || len(title.usage) != 1 || title.usage[0].cost != 0.002 || !title.usage[0].known {
		t.Fatalf("%+v", title)
	}
	if h.session() != s || h.f.launchCount() != 1 {
		t.Fatal("the pooled session was touched")
	}
	// And it still continues.
	r2 := h.run(context.Background(), next(h1, r1, result("tu1", "R")))
	if r2.err != nil || r2.text != "answer:R" || h.f.launchCount() != 1 {
		t.Fatalf("%+v", r2)
	}
	// With tools but nowhere to keep a process, the request is refused rather than answered without them.
	h.m.SessionKey = ""
	if r := h.run(context.Background(), h1); r.err == nil {
		t.Fatal("a tool request with no session key ran")
	}
}

func TestClaudeModelIdleSessionIsReapedAndItsCallsFail(t *testing.T) {
	SetPoolLimits(100*time.Millisecond, 0)
	t.Cleanup(func() { SetPoolLimits(0, 0) })
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}} })
	h1 := []agent.Message{user("hi")}
	r1 := h.run(context.Background(), h1)
	if r1.err != nil {
		t.Fatal(r1.err)
	}
	gone(t, h.f, 0)
	for i := 0; i < 100 && h.session() != nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if h.session() != nil {
		t.Fatal("idle session not reaped")
	}
	// The results arrive too late: a new process, seeded with the transcript.
	r2 := h.run(context.Background(), next(h1, r1, result("tu1", "R")))
	if r2.err != nil || h.f.launchCount() != 2 {
		t.Fatalf("%v launches %d", r2.err, h.f.launchCount())
	}
}

func TestClaudeModelHeldCallsAreFailedWhenReaped(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}, noCall: true} })
	r1 := h.run(context.Background(), []agent.Message{user("hi")})
	if r1.err != nil {
		t.Fatal(r1.err)
	}
	e := h.f.endpoint(t, 0)
	held := make(chan string, 1)
	go func() { _, rerr, err := e.call(context.Background(), "tu1"); held <- cmpOr(rerr, fmt.Sprint(err)) }()
	time.Sleep(100 * time.Millisecond)
	if _, rerr, _ := e.call(context.Background(), "unknown"); !strings.Contains(rerr, "unknown tool call") {
		t.Fatalf("unknown id: %q", rerr)
	}
	nativePoolIdle := h.session()
	nativePoolIdle.close() // what the idle timer does
	select {
	case rerr := <-held:
		if rerr == "" || rerr == "<nil>" {
			t.Fatalf("held call got %q", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held call not released")
	}
	if _, _, err := e.call(context.Background(), "tu1"); err == nil {
		t.Fatal("a gone session still answers")
	}
}

func TestClaudeModelAHeldConnectionClosingDropsTheSession(t *testing.T) {
	h := newHarness(t, func(string) cliPlan { return cliPlan{rounds: [][]string{{"tu1"}}, noCall: true} })
	r1 := h.run(context.Background(), []agent.Message{user("hi")})
	if r1.err != nil {
		t.Fatal(r1.err)
	}
	e := h.f.endpoint(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go e.call(ctx, "tu1")
	time.Sleep(100 * time.Millisecond)
	cancel() // the CLI gave up on the call
	gone(t, h.f, 0)
	r2 := h.run(context.Background(), next([]agent.Message{user("hi")}, r1, result("tu1", "R")))
	if r2.err != nil || h.f.launchCount() != 2 {
		t.Fatalf("%v launches %d", r2.err, h.f.launchCount())
	}
}

func TestClaudeFreshPrompt(t *testing.T) {
	if p := claudeFreshPrompt([]agent.Message{user("hi")}); p != "hi" {
		t.Fatal(p)
	}
	var _ = bufio.NewScanner
}

// A CLI's stderr stays on the machine that ran it: what goes to another server leaves it out.
func TestWithoutStderr(t *testing.T) {
	err := fmt.Errorf("forwarded: %w", &stderrErr{errors.New("claude exited without finishing the round"), "secret path /home/me"})
	if got := WithoutStderr(err); got != "forwarded: claude exited without finishing the round" {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(err.Error(), "secret path") {
		t.Fatal("the local error lost its stderr")
	}
}
