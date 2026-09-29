package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

// One request/response round against a fake Messages API: what goes out
// (cache breakpoints, grouped tool results, verbatim replay, thinking
// config) and what comes back (raw blocks for replay, cache usage).
func TestAnthropicStreamTurn_CachingThinkingAndReplay(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("content-type", "text/event-stream")
		for _, ev := range []string{
			`event: message_start
data: {"message":{"usage":{"input_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":90,"output_tokens":1}}}`,
			`event: content_block_start
data: {"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`event: content_block_delta
data: {"index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			`event: content_block_delta
data: {"index":0,"delta":{"type":"signature_delta","signature":"SIG"}}`,
			`event: content_block_stop
data: {"index":0}`,
			`event: content_block_start
data: {"index":1,"content_block":{"type":"tool_use","id":"tu1","name":"file_read","input":{}}}`,
			`event: content_block_delta
data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`,
			`event: content_block_stop
data: {"index":1}`,
			`event: message_delta
data: {"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		} {
			fmt.Fprintf(w, "%s\n\n", ev)
		}
	}))
	defer srv.Close()

	p := NewAnthropic("k", "claude-opus-5")
	p.BaseURL = srv.URL
	p.SetModelOptions(agent.ModelOptions{ReasoningEffort: "high"})

	replayed := json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"x1","name":"search","input":{"pattern":"y"}}]`)
	history := []agent.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", RawContent: replayed, ToolCalls: []agent.ToolCall{{ID: "x1", Name: "search"}, {ID: "x2", Name: "search"}}},
		{Role: "tool", ToolCallID: "x1", Content: "r1"},
		{Role: "tool", ToolCallID: "x2", Content: "r2"},
	}
	tools := []tool.ToolDefinition{{Name: "file_read", Parameters: map[string]any{"type": "object"}}}
	chunks, errs := p.StreamTurn(context.Background(), history, tools, "sys")

	var raw json.RawMessage
	var calls []*agent.ToolCall
	var usage agent.ModelStreamChunk
	for c := range chunks {
		switch c.Kind {
		case agent.StreamChunkRawContent:
			raw = c.Raw
		case agent.StreamChunkToolCall:
			calls = append(calls, c.ToolCall)
		case agent.StreamChunkUsage:
			usage = c
		}
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}

	// Request shape.
	if th, _ := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", body["thinking"])
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v, want effort high", body["output_config"])
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("system = %v, want one block with a cache breakpoint", body["system"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want user, assistant, one user holding both tool results; got %d messages", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["signature"] != "s" {
		t.Errorf("assistant raw content not replayed verbatim: %v", asst)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	if len(results) != 2 || results[1].(map[string]any)["cache_control"] == nil || results[0].(map[string]any)["cache_control"] != nil {
		t.Errorf("tool results = %v, want both in one message with the breakpoint on the last", results)
	}

	// Response handling.
	if len(calls) != 1 || string(calls[0].Args) != `{"path":"a"}` {
		t.Errorf("tool calls = %+v", calls)
	}
	if !strings.Contains(string(raw), `"signature":"SIG"`) || !strings.Contains(string(raw), `"input":{"path":"a"}`) {
		t.Errorf("raw content = %s, want the thinking block with its signature and the tool input", raw)
	}
	if u := usage.Usage; u == nil || u.InputTokens != 1000 || u.CachedInputTokens != 900 || u.CacheWriteInputTokens != 90 || u.OutputTokens != 20 {
		t.Errorf("usage = %+v, want input 1000 (900 read, 90 written), output 20", usage.Usage)
	}
}

func TestAdaptiveThinking(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-5-5": true, "claude-sonnet-5": true, "claude-opus-4-6": true, "claude-fable-5-1": true,
		"claude-haiku-4-5": false, "claude-sonnet-4-5-20250929": false, "claude-opus-4-1-20250805": false,
		"claude-sonnet-4-20250514": false, "claude-3-5-sonnet-20241022": false,
	} {
		if got := adaptiveThinking(model); got != want {
			t.Errorf("adaptiveThinking(%q) = %v, want %v", model, got, want)
		}
	}
}
