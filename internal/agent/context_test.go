package agent

import (
	"strings"
	"testing"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// The view only changes when a compaction is appended: that's what keeps
// consecutive requests on one cached prefix.
func TestContextView_AppliesRecordedCompactions(t *testing.T) {
	big := strings.Repeat("x", 4000)
	history := []Message{
		{ID: "m1", Role: "user", Content: "fix the bug"},
		{ID: "m2", Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "file_read", Args: []byte(`{"path":"a.go"}`)}}},
		{ID: "m3", Role: "tool", ToolCallID: "c1", Content: big},
		{ID: "m4", Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "file_read", Args: []byte(`{"path":"b.go"}`)}}},
		{ID: "m5", Role: "tool", ToolCallID: "c2", Content: big},
	}
	cm := NewContextManager(2000, 4000)

	before := contextView(history, "k")
	if len(before) != 5 || before[2].Content != big {
		t.Fatalf("no compaction yet: expected history unchanged, got %+v", before)
	}

	clearBefore, saved, ok := cm.planClear(history, 2600, 2000)
	if !ok || clearBefore != "m4" || saved < 900 {
		t.Fatalf("expected to clear up to the latest round (m4), got %q saved=%d ok=%v", clearBefore, saved, ok)
	}
	history = append(history, Message{ID: "m6", Role: "compaction", Compaction: &Compaction{Kind: CompactionClear, ClearBefore: clearBefore}})
	view := contextView(history, "k")
	if len(view) != 5 || !strings.HasPrefix(view[2].Content, "[cleared") || view[4].Content != big {
		t.Fatalf("expected the old result stubbed and the latest kept, got %+v", view)
	}

	history = append(history, Message{ID: "m7", Role: "compaction", Content: "did things", Compaction: &Compaction{Kind: CompactionSummary}},
		Message{ID: "m8", Role: "assistant", Content: "continuing"})
	view = contextView(history, "k")
	if len(view) != 2 || view[0].Role != "user" || !strings.Contains(view[0].Content, "did things") || view[1].Content != "continuing" {
		t.Fatalf("expected summary + what followed it, got %+v", view)
	}
}

// Raw content (Anthropic thinking blocks) replays only on the trailing run
// generated under the current prefix and after the last compaction; blocks
// may only be dropped from the front.
func TestContextView_RawContentReplayRules(t *testing.T) {
	raw := []byte(`[{"type":"thinking"}]`)
	history := []Message{
		{ID: "m1", Role: "user", Content: "hi"},
		{ID: "m2", Role: "assistant", Content: "a", RawContent: raw, PrefixKey: "k"},
		{ID: "m3", Role: "user", Content: "switched mode"},
		{ID: "m4", Role: "assistant", Content: "b", RawContent: raw, PrefixKey: "other"},
		{ID: "m5", Role: "user", Content: "back"},
		{ID: "m6", Role: "assistant", Content: "c", RawContent: raw, PrefixKey: "k"},
	}
	view := contextView(history, "k")
	if view[1].RawContent != nil || view[3].RawContent != nil || view[5].RawContent == nil {
		t.Fatalf("expected only the trailing same-prefix message to keep raw content")
	}

	history = append(history, Message{ID: "m7", Role: "compaction", Compaction: &Compaction{Kind: CompactionClear, ClearBefore: "m2"}})
	if view := contextView(history, "k"); view[5].RawContent != nil {
		t.Fatal("expected raw content from before a compaction to be dropped")
	}
}

func TestContextTokens_PrefersReportedUsageSinceLastCompaction(t *testing.T) {
	cm := NewContextManager(1000, 100)
	history := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok", Usage: &protocol.UsageData{InputTokens: 900, OutputTokens: 100}},
		{Role: "tool", Content: strings.Repeat("y", 400)},
	}
	if got, src := cm.contextTokens(history, history, 0); got != 1100 || src != "provider" {
		t.Fatalf("expected reported 1000 + 100 estimated from the provider, got %d (%s)", got, src)
	}
	history = append(history, Message{Role: "compaction", Compaction: &Compaction{Kind: CompactionClear}})
	if got, src := cm.contextTokens(history, history[:3], 50); got >= 1000 || src != "estimated" {
		t.Fatalf("expected a fresh estimate after compaction, got %d (%s)", got, src)
	}
}

func TestContextTokens_Sources(t *testing.T) {
	cm := NewContextManager(1000, 100)
	user := Message{Role: "user", Content: strings.Repeat("u", 400)}
	tests := []struct {
		name    string
		history []Message
		want    int
		source  string
	}{
		{"api reply: input plus output", []Message{user, {Role: "assistant", Usage: &protocol.UsageData{InputTokens: 900, OutputTokens: 100}}}, 1000, "provider"},
		{"native aggregate with a snapshot uses the snapshot", []Message{user, {Role: "assistant", UsageIsAggregate: true,
			Usage: &protocol.UsageData{InputTokens: 900_000, OutputTokens: 500, ContextTokens: 50_000}}}, 50_000, "provider"},
		{"native aggregate without a snapshot is spend, not context", []Message{user, {Role: "assistant", UsageIsAggregate: true,
			Usage: &protocol.UsageData{InputTokens: 900_000, OutputTokens: 500}}}, 100, "estimated"},
		{"what came after the reply is estimated on top", []Message{user, {Role: "assistant", Usage: &protocol.UsageData{ContextTokens: 700}}, {Role: "tool", Content: strings.Repeat("t", 400)}}, 800, "provider"},
		{"nothing yet", nil, 0, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, src := cm.contextTokens(tt.history, contextView(tt.history, ""), 0)
			if got != tt.want || src != tt.source {
				t.Fatalf("got %d (%s), want %d (%s)", got, src, tt.want, tt.source)
			}
		})
	}

	// Nothing before a compaction counts, the snapshot included.
	history := []Message{user, {Role: "assistant", Usage: &protocol.UsageData{ContextTokens: 9000}}, {Role: "compaction", Compaction: &Compaction{Kind: CompactionClear}}}
	if got, src := cm.contextTokens(history, []Message{user}, 50); got != 150 || src != "estimated" {
		t.Fatalf("after compaction got %d (%s), want 150 (estimated)", got, src)
	}
}

func TestWindow_Precedence(t *testing.T) {
	t.Cleanup(func() { SetContextWindowLookup(nil) })
	SetContextWindowLookup(func(model string) int {
		if model == "m" {
			return 200_000
		}
		return 0
	})
	reported := []Message{{Role: "assistant", Model: "m", Usage: &protocol.UsageData{WindowTokens: 1_000_000}}}

	cm := NewContextManager(64000, 100)
	if got := cm.Window("unlisted", nil); got != 0 {
		t.Fatalf("unknown model: got %d, want 0", got)
	}
	if got := cm.Window("m", nil); got != 200_000 {
		t.Fatalf("catalog: got %d, want 200000", got)
	}
	if got := cm.Window("m", reported); got != 1_000_000 {
		t.Fatalf("provider-reported beats the catalog: got %d", got)
	}
	if got := cm.Window("other", reported); got != 0 {
		t.Fatalf("a window reported by another model doesn't apply: got %d", got)
	}
	cm.SetContextWindow(32_000)
	if got := cm.Window("m", reported); got != 32_000 {
		t.Fatalf("user override beats everything: got %d", got)
	}
}

func TestReport_MaxIsWindowAndBudgetIsClamped(t *testing.T) {
	t.Cleanup(func() { SetContextWindowLookup(nil) })
	history := []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Model: "m", Usage: &protocol.UsageData{InputTokens: 10, OutputTokens: 5}}}

	SetContextWindowLookup(func(string) int { return 0 })
	cm := NewContextManager(64000, 100)
	r := cm.Report(history, "m", 0)
	if r.MaxTokens != 64000 || r.BudgetTokens != 64000 || r.WindowTokens != 0 || r.EstimatedTokens != 15 || r.Source != "provider" {
		t.Fatalf("unknown window: %+v", r)
	}

	SetContextWindowLookup(func(string) int { return 1_000_000 })
	if r := cm.Report(history, "m", 0); r.MaxTokens != 1_000_000 || r.WindowTokens != 1_000_000 || r.BudgetTokens != 64000 {
		t.Fatalf("large window keeps the configured budget: %+v", r)
	}

	SetContextWindowLookup(func(string) int { return 32_000 })
	if r := cm.Report(history, "", 0); r.MaxTokens != 32_000 || r.BudgetTokens != 28_800 {
		t.Fatalf("a small window (model taken from the history) caps the budget at 90%%: %+v", r)
	}

	// What maybeCompact compares against is what the meter reports.
	if got := NewContextManager(100, 100).Report(history, "m", 0).BudgetTokens; got != minCompactionBudget {
		t.Fatalf("budget below the minimum: got %d, want %d", got, minCompactionBudget)
	}
}

func TestEstimateTokens_RuneAware(t *testing.T) {
	cm := NewContextManager(0, 0)
	if got := cm.EstimateTokens(strings.Repeat("a", 400)); got != 100 {
		t.Errorf("ascii: got %d, want 100", got)
	}
	cjk := strings.Repeat("字", 300) // 900 bytes: byte/4 would say 225
	if got := cm.EstimateTokens(cjk); got != 200 {
		t.Errorf("cjk: got %d, want 200 (1 token per 1.5 chars)", got)
	}
	if got := cm.EstimateTokens(""); got != 0 {
		t.Errorf("empty: got %d", got)
	}
}

func TestEstimateHistoryTokens_CountsReasoningRawContentAndAttachments(t *testing.T) {
	cm := NewContextManager(0, 0)
	text := strings.Repeat("x", 400) // 100 tokens
	tests := []struct {
		name string
		m    Message
		want int
	}{
		{"content", Message{Content: text}, 100},
		{"reasoning", Message{Reasoning: text}, 100},
		{"raw content stands in for the reasoning it holds", Message{Reasoning: text, RawContent: []byte(text)}, 100},
		{"tool call args", Message{ToolCalls: []ToolCall{{Args: []byte(text)}}}, 100},
		{"image attachment gets a flat allowance", Message{Attachments: []string{"/tmp/shot.PNG"}}, imageTokens + 4},
		{"other attachments only cost their note", Message{Attachments: []string{"/tmp/a.txt"}}, 3},
	}
	for _, tt := range tests {
		if got := cm.EstimateHistoryTokens([]Message{tt.m}); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestEstimateOverhead_UsesTheWireJSON(t *testing.T) {
	cm := NewContextManager(0, 0)
	defs := []tool.ToolDefinition{{Name: "t", Description: strings.Repeat("d", 400), Parameters: map[string]any{"type": "object"}}}
	if got := cm.estimateOverhead(strings.Repeat("s", 40), defs); got < 110 {
		t.Fatalf("overhead %d should cover the 10-token prompt and the 100+ token definition", got)
	}
}

func TestPriceUsage_UnknownModelIsFlaggedNotFree(t *testing.T) {
	used := protocol.UsageData{InputTokens: 1000, OutputTokens: 100}

	u := used
	priceUsage("some-unlisted-local-model", &u)
	if !u.CostUnknown || u.CostKnown || u.CostUSD != 0 {
		t.Fatalf("unpriced model: %+v", u)
	}

	u = used
	priceUsage("gpt-4o", &u)
	if u.CostUnknown || !u.CostKnown || u.CostUSD <= 0 {
		t.Fatalf("priced model: %+v", u)
	}

	u = used
	priceUsage("", &u) // a provider with no model name can't be priced either
	if !u.CostUnknown {
		t.Fatalf("no model: %+v", u)
	}

	u = protocol.UsageData{CostUSD: 0.5, CostKnown: true, InputTokens: 10}
	priceUsage("some-unlisted-local-model", &u)
	if u.CostUnknown || u.CostUSD != 0.5 {
		t.Fatalf("a provider-reported cost is left alone: %+v", u)
	}

	u = protocol.UsageData{}
	priceUsage("some-unlisted-local-model", &u)
	if u.CostUnknown {
		t.Fatalf("no tokens, nothing to price: %+v", u)
	}
}

func TestAddUsage_CostUnknownAndSnapshots(t *testing.T) {
	var total protocol.UsageData
	addUsage(&total, protocol.UsageData{InputTokens: 10, ContextTokens: 500, WindowTokens: 1000})
	addUsage(&total, protocol.UsageData{InputTokens: 10, CostUnknown: true, ContextTokens: 700})
	addUsage(&total, protocol.UsageData{InputTokens: 10})
	if total.InputTokens != 30 || !total.CostUnknown || total.ContextTokens != 700 || total.WindowTokens != 1000 {
		t.Fatalf("got %+v", total)
	}
}
