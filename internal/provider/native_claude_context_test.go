package provider

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// A get_context_usage answer as the installed Claude Code (2.1.284) gave it
// for a fresh chat, trimmed to the fields we read: the Messages row is 4787
// tokens while messageBreakdown lists 13.5k (it counts attachments the
// other rows already hold), so the split has to be scaled and can't be exact.
const claudeContextFixture = `{
  "categories": [
    {"name": "System prompt", "tokens": 7019, "kind": "used"},
    {"name": "System tools", "tokens": 38163, "kind": "used"},
    {"name": "System tools (deferred)", "tokens": 38558, "isDeferred": true, "kind": "deferred"},
    {"name": "Memory files", "tokens": 57, "kind": "used"},
    {"name": "Skills", "tokens": 1984, "kind": "used"},
    {"name": "Messages", "tokens": 4787, "kind": "used"},
    {"name": "Free space", "tokens": 147990, "kind": "free"}
  ],
  "totalTokens": 52010, "maxTokens": 200000, "rawMaxTokens": 200000,
  "model": "claude-haiku-4-5-20251001",
  "autoCompactThreshold": 167000, "isAutoCompactEnabled": true,
  "messageBreakdown": {"toolCallTokens": 0, "toolResultTokens": 0, "attachmentTokens": 13479, "assistantMessageTokens": 24, "userMessageTokens": 14, "redirectedContextTokens": 0, "unattributedTokens": 0},
  "apiUsage": {"input_tokens": 6, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 1500, "output_tokens": 9}
}`

func sumTokens(cats []protocol.ContextCategory) (used, spare int) {
	for _, c := range cats {
		if c.Key == protocol.CtxFree || c.Key == protocol.CtxBuffer {
			spare += c.Tokens
		} else {
			used += c.Tokens
		}
	}
	return
}

func TestParseClaudeContextUsage(t *testing.T) {
	out, err := parseClaudeContextUsage(json.RawMessage(claudeContextFixture), true)
	if err != nil {
		t.Fatal(err)
	}
	if out.TotalTokens != 52010 || out.WindowTokens != 200000 || out.AutoCompactAt != 167000 || out.Source != "provider" || out.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("header = %+v", out)
	}
	if out.Cache == nil || out.Cache.Read != 1500 || out.Cache.Write != 20 || out.Cache.Input != 1526 {
		t.Fatalf("cache = %+v", out.Cache)
	}
	used, spare := sumTokens(out.Categories)
	if used != 52010 || spare != 147990 {
		t.Fatalf("used %d spare %d in %+v", used, spare, out.Categories)
	}
	by := map[string]protocol.ContextCategory{}
	for _, c := range out.Categories {
		by[c.Key] = c
	}
	if by[protocol.CtxSystem].Tokens != 7019 || !by[protocol.CtxSystem].Exact || by[protocol.CtxTools].Tokens != 38163 || by[protocol.CtxMemory].Tokens != 57 || by[protocol.CtxSkills].Tokens != 1984 {
		t.Fatalf("rows = %+v", out.Categories)
	}
	if _, ok := by[protocol.CtxMessages]; ok {
		t.Fatalf("messages row not split: %+v", out.Categories)
	}
	if att := by[protocol.CtxAttachments]; att.Tokens < 4700 || att.Exact {
		t.Fatalf("a split that disagrees with its row is an estimate: %+v", att)
	}
	for _, c := range out.Categories {
		if c.Label == "" {
			t.Fatalf("category without a label: %+v", c)
		}
	}
}

func TestParseClaudeContextUsage_SummaryIsNotExactAndAgreeingSplitIs(t *testing.T) {
	raw := `{"categories":[{"name":"Messages","tokens":1000,"kind":"used"},{"name":"Plugin thing","tokens":10,"kind":"used"},{"name":"Autocompact buffer","tokens":33000,"kind":"buffer"}],
	 "totalTokens":1010,"maxTokens":200000,"rawMaxTokens":200000,"isAutoCompactEnabled":false,
	 "messageBreakdown":{"toolCallTokens":100,"toolResultTokens":600,"attachmentTokens":0,"assistantMessageTokens":150,"userMessageTokens":150,"redirectedContextTokens":0,"unattributedTokens":0}}`
	out, err := parseClaudeContextUsage(json.RawMessage(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != "estimated" || out.AutoCompactAt != 0 || out.Cache != nil {
		t.Fatalf("no API response yet: %+v", out)
	}
	for _, c := range out.Categories {
		if c.Exact {
			t.Fatalf("summary detail claims exactness: %+v", c)
		}
	}
	last := out.Categories[len(out.Categories)-1]
	if last.Key != protocol.CtxBuffer || last.Tokens != 33000 {
		t.Fatalf("buffer = %+v", last)
	}
	var other protocol.ContextCategory
	for _, c := range out.Categories {
		if c.Key == protocol.CtxOther {
			other = c
		}
	}
	if other.Label != "Plugin thing" || other.Tokens != 10 {
		t.Fatalf("an unknown row keeps its own name: %+v", out.Categories)
	}
	used, _ := sumTokens(out.Categories)
	if used != 1010 {
		t.Fatalf("used = %d", used)
	}
}

// A chat whose API hasn't answered yet (or whose login lapsed) gets the CLI's local estimates
// even when "full" is asked for; claiming exactness there would be wrong.
func TestParseClaudeContextUsage_NoAPIResponseIsNotExact(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(claudeContextFixture), &body)
	body["apiUsage"] = nil
	raw, _ := json.Marshal(body)
	out, err := parseClaudeContextUsage(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Categories {
		if c.Exact {
			t.Fatalf("exact without an API response: %+v", c)
		}
	}
	if out.Source != "estimated" || out.Cache != nil {
		t.Fatalf("%+v", out)
	}
}

func TestParseClaudeContextUsage_Empty(t *testing.T) {
	if _, err := parseClaudeContextUsage(json.RawMessage(`{}`), true); err == nil {
		t.Fatal("an empty answer is an error, not a blank breakdown")
	}
}

// The breakdown goes to the chat's live process, asking for the exact count
// first, and falls back to the cheap one when that fails.
func TestClaudeContextBreakdownAsksTheLiveProcess(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	base := claudeScript(0.1)
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir(), Start: f.starter(func(l int, m map[string]any, out io.Writer) {
		if m["type"] == "control_request" && controlSubtype(m) == "get_context_usage" {
			id, _ := m["request_id"].(string)
			req, _ := m["request"].(map[string]any)
			if req["detail"] == "full" {
				line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "token count unavailable"}})
				return
			}
			var body map[string]any
			_ = json.Unmarshal([]byte(claudeContextFixture), &body)
			line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": body}})
			return
		}
		base(l, m, out)
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if got, err := p.ContextBreakdown(ctx, key); got != nil || err != nil {
		t.Fatalf("no process yet, nothing to ask: %+v %v", got, err)
	}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "hi")); err != nil {
		t.Fatal(err)
	}
	got, err := p.ContextBreakdown(ctx, key)
	if err != nil || got == nil {
		t.Fatalf("breakdown = %+v, %v", got, err)
	}
	if got.TotalTokens != 52010 {
		t.Fatalf("total = %d", got.TotalTokens)
	}
	if got.Categories[0].Exact {
		t.Fatalf("the fallback to summary must not claim exactness: %+v", got.Categories[0])
	}
	asked := f.controls("get_context_usage")
	if len(asked) != 2 || asked[0]["request"].(map[string]any)["detail"] != "full" || asked[1]["request"].(map[string]any)["detail"] != "summary" {
		t.Fatalf("asked %v", asked)
	}
	// The other providers have nothing to ask.
	codex := &CLISubprocessProvider{Kind: CLIKindCodex}
	if got, err := codex.ContextBreakdown(ctx, key); got != nil || err != nil {
		t.Fatalf("codex = %+v %v", got, err)
	}
}
