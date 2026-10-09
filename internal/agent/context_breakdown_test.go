package agent

import (
	"context"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

func catMap(cats []protocol.ContextCategory) map[string]protocol.ContextCategory {
	m := map[string]protocol.ContextCategory{}
	for _, c := range cats {
		m[c.Key] = c
	}
	return m
}

func usedSum(cats []protocol.ContextCategory) (used, spare int) {
	for _, c := range cats {
		if c.Key == protocol.CtxFree || c.Key == protocol.CtxBuffer {
			spare += c.Tokens
		} else {
			used += c.Tokens
		}
	}
	return
}

func TestScaleShares_SumsExactlyAndSkipsEmpty(t *testing.T) {
	for _, total := range []int{1, 7, 100, 12345, 999_999} {
		cats := ScaleShares(map[string]int{protocol.CtxSystem: 3, protocol.CtxTools: 5, protocol.CtxUser: 1, protocol.CtxAssistant: 0, protocol.CtxToolResults: 13}, total)
		used, _ := usedSum(cats)
		if used != total {
			t.Fatalf("total %d: categories add up to %d (%+v)", total, used, cats)
		}
		for _, c := range cats {
			if c.Tokens <= 0 || c.Key == protocol.CtxAssistant || c.Exact || c.Label == "" {
				t.Fatalf("total %d: bad category %+v", total, c)
			}
		}
	}
	if got := ScaleShares(nil, 100); got != nil {
		t.Fatalf("nothing to split: %+v", got)
	}
	if got := ScaleShares(map[string]int{protocol.CtxUser: 4}, 0); got != nil {
		t.Fatalf("nothing to fill: %+v", got)
	}
}

func breakdownHistory() []Message {
	return []Message{
		{ID: "m1", Role: "user", Content: strings.Repeat("u", 400), Attachments: []string{"/tmp/a.png", "/tmp/b.txt"}},
		{ID: "m2", Role: "assistant", Content: strings.Repeat("a", 800), Reasoning: strings.Repeat("r", 1200),
			ToolCalls: []ToolCall{{ID: "c1", Name: "file_read", Args: []byte(`{"path":"a.go"}`)}}},
		{ID: "m3", Role: "tool", ToolCallID: "c1", Content: strings.Repeat("t", 8000)},
		{ID: "m4", Role: "assistant", Content: "done", Usage: &protocol.UsageData{InputTokens: 6000, OutputTokens: 40, CachedInputTokens: 4000, CacheWriteInputTokens: 500}},
	}
}

var oneTool = []tool.ToolDefinition{{Name: "file_read", Description: strings.Repeat("d", 2000), Parameters: map[string]any{"type": "object"}}}

// The provider's count is the total; our estimates only decide how it is split.
func TestBreakdown_APIProviderScalesEstimatesToReportedTotal(t *testing.T) {
	t.Cleanup(func() { SetContextWindowLookup(nil) })
	SetContextWindowLookup(func(string) int { return 200_000 })
	cm := NewContextManager(64000, 4000)
	got := cm.Breakdown(breakdownHistory(), "m", strings.Repeat("s", 4000), oneTool, false)

	if got.Source != "provider" || got.TotalTokens != 6040 || got.WindowTokens != 200_000 || got.AutoCompactAt != 64000 {
		t.Fatalf("header = %+v", got)
	}
	used, spare := usedSum(got.Categories)
	if used != 6040 {
		t.Fatalf("used categories add up to %d, want the reported 6040: %+v", used, got.Categories)
	}
	// Free is the room before compaction; the buffer is the rest of the window.
	if spare != 200_000-6040 {
		t.Fatalf("free+buffer = %d: %+v", spare, got.Categories)
	}
	by := catMap(got.Categories)
	if by[protocol.CtxFree].Tokens != 64000-6040 || by[protocol.CtxBuffer].Tokens != 200_000-64000 {
		t.Fatalf("free/buffer = %+v / %+v", by[protocol.CtxFree], by[protocol.CtxBuffer])
	}
	for _, key := range []string{protocol.CtxSystem, protocol.CtxTools, protocol.CtxUser, protocol.CtxAssistant, protocol.CtxReasoning, protocol.CtxToolCalls, protocol.CtxToolResults, protocol.CtxAttachments} {
		if by[key].Tokens <= 0 {
			t.Fatalf("no %s in %+v", key, got.Categories)
		}
	}
	// 8000 bytes of tool output dominates 400 of user text.
	if by[protocol.CtxToolResults].Tokens <= by[protocol.CtxUser].Tokens*3 {
		t.Fatalf("shares don't follow size: %+v", got.Categories)
	}
	for _, c := range got.Categories {
		if c.Exact {
			t.Fatalf("an apportioned figure is not exact: %+v", c)
		}
	}
	if got.Cache == nil || got.Cache.Read != 4000 || got.Cache.Write != 500 || got.Cache.Input != 6000 {
		t.Fatalf("cache = %+v", got.Cache)
	}
}

func TestBreakdown_EstimatedWhenProviderNeverReported(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := breakdownHistory()
	h[3].Usage = nil
	got := cm.Breakdown(h, "", "sys", oneTool, false)
	rep := cm.Report(h, "", cm.estimateOverhead("sys", oneTool))
	if got.Source != "estimated" || got.TotalTokens != rep.EstimatedTokens {
		t.Fatalf("breakdown %+v disagrees with the meter %+v", got, rep)
	}
	if used, _ := usedSum(got.Categories); used != got.TotalTokens {
		t.Fatalf("used = %d, total = %d", used, got.TotalTokens)
	}
	if got.WindowTokens != 0 || catMap(got.Categories)[protocol.CtxBuffer].Tokens != 0 {
		t.Fatalf("no window known, so no buffer: %+v", got)
	}
	if got.Cache != nil {
		t.Fatalf("no usage, no cache figure: %+v", got.Cache)
	}
}

func TestBreakdown_BeforeFirstMessageShowsTheFixedPart(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	got := cm.Breakdown(nil, "m", "you are helpful", oneTool, false)
	by := catMap(got.Categories)
	if got.Source != "estimated" || by[protocol.CtxSystem].Tokens == 0 || by[protocol.CtxTools].Tokens == 0 || by[protocol.CtxUser].Tokens != 0 {
		t.Fatalf("%+v", got)
	}
	if used, _ := usedSum(got.Categories); used != got.TotalTokens {
		t.Fatalf("used = %d, total = %d", used, got.TotalTokens)
	}
}

func TestBreakdown_SummaryAfterCompactionAndOverBudget(t *testing.T) {
	cm := NewContextManager(8000, 4000)
	h := []Message{
		{ID: "m1", Role: "user", Content: strings.Repeat("old ", 500)},
		{ID: "m2", Role: "assistant", Content: "old reply", Usage: &protocol.UsageData{InputTokens: 9000, OutputTokens: 100}},
		{ID: "m3", Role: "compaction", Content: strings.Repeat("summary ", 100), Compaction: &Compaction{Kind: CompactionSummary}},
		{ID: "m4", Role: "user", Content: "next"},
	}
	got := cm.Breakdown(h, "", "sys", oneTool, false)
	by := catMap(got.Categories)
	if got.Source != "estimated" || by[protocol.CtxSummary].Tokens == 0 || by[protocol.CtxUser].Tokens == 0 {
		t.Fatalf("%+v", got)
	}
	if by[protocol.CtxUser].Tokens >= by[protocol.CtxSummary].Tokens {
		t.Fatalf("the summary is the big part: %+v", got.Categories)
	}

	// Past the budget there is no free space left, but the window still has a buffer.
	t.Cleanup(func() { SetContextWindowLookup(nil) })
	SetContextWindowLookup(func(string) int { return 20_000 })
	h = []Message{{Role: "user", Content: "q"}, {Role: "assistant", Usage: &protocol.UsageData{InputTokens: 9500, OutputTokens: 500}}}
	got = NewContextManager(8000, 100).Breakdown(h, "m", "sys", nil, false)
	by = catMap(got.Categories)
	if _, has := by[protocol.CtxFree]; has || by[protocol.CtxBuffer].Tokens != 20_000-10_000 {
		t.Fatalf("over the budget: %+v", got.Categories)
	}
}

// A native agent has its own system prompt and tools, so only the total is
// known — and none of yoakeland's compaction applies.
func TestBreakdown_NativeIsTotalOnly(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := []Message{{Role: "user", Content: "hi"}, {Role: "assistant", UsageIsAggregate: true, Usage: &protocol.UsageData{InputTokens: 900_000, ContextTokens: 42_000, WindowTokens: 200_000}}}
	got := cm.Breakdown(h, "", "", nil, true)
	if got.Source != "provider" || got.TotalTokens != 42_000 || got.AutoCompactAt != 0 || got.WindowTokens != 200_000 {
		t.Fatalf("%+v", got)
	}
	if len(got.Categories) != 2 || got.Categories[0].Key != protocol.CtxOther || !got.Categories[0].Exact || got.Categories[1].Key != protocol.CtxFree || got.Categories[1].Tokens != 158_000 {
		t.Fatalf("%+v", got.Categories)
	}
	if got.Cache != nil {
		t.Fatalf("a turn's summed cache reads say nothing about the last request: %+v", got.Cache)
	}

	got = cm.Breakdown(nil, "", "", nil, true)
	if got.Source != "unknown" || len(got.Categories) != 0 {
		t.Fatalf("nothing to measure: %+v", got)
	}
}

// The live Agent path: spend comes from the usage store, the CLI's own
// numbers replace the estimate, and the session id is stamped on.
type inspectingProvider struct {
	own *protocol.ContextBreakdownData
	key string
}

func (*inspectingProvider) RunsOwnTools() {}

func (*inspectingProvider) StreamTurn(context.Context, []Message, []tool.ToolDefinition, string) (<-chan ModelStreamChunk, <-chan error) {
	c, e := make(chan ModelStreamChunk), make(chan error)
	close(c)
	close(e)
	return c, e
}

func (p *inspectingProvider) ContextBreakdown(_ context.Context, key string) (*protocol.ContextBreakdownData, error) {
	p.key = key
	return p.own, nil
}

func TestAgentContextBreakdown_UsesTheAgentsOwnCount(t *testing.T) {
	store, err := NewUsageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Record("s1", 10, 5, 1.25)
	p := &inspectingProvider{own: &protocol.ContextBreakdownData{TotalTokens: 52010, Source: "provider",
		Categories: []protocol.ContextCategory{{Key: protocol.CtxSystem, Label: "System prompt", Tokens: 7019, Exact: true}}}}
	ag := NewAgent(nil, p, "claude", nil, nil, nil, nil, NewContextManager(64000, 4000), store)
	ag.sessionID, ag.projectRoot = "s1", "/proj"
	ag.history = []Message{{Role: "user", Content: "hi"}}

	got := ag.ContextBreakdown(context.Background())
	if got.SessionID != "s1" || got.TotalTokens != 52010 || got.Categories[0].Tokens != 7019 || !got.Categories[0].Exact {
		t.Fatalf("%+v", got)
	}
	if p.key != NativeSessionKey("s1", "/proj") {
		t.Fatalf("asked for %q", p.key)
	}
	if got.Spend == nil || got.Spend.SessionTotalUSD != 1.25 {
		t.Fatalf("spend = %+v", got.Spend)
	}

	// A process that isn't there to ask leaves the total-only answer.
	p.own = nil
	got = ag.ContextBreakdown(context.Background())
	if got.Source != "estimated" || len(got.Categories) != 1 || got.Categories[0].Key != protocol.CtxOther || got.Categories[0].Exact || got.SessionID != "s1" {
		t.Fatalf("%+v", got)
	}
}

// A native reply saved before UsageIsAggregate existed holds a whole turn's
// summed usage; LoadSession must not let it read as the context size.
func TestLoadSession_LegacyNativeUsageIsNotTheContext(t *testing.T) {
	store, err := NewDiskSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := &SessionState{SessionID: "s1", Native: &NativeSessionRef{Provider: "claude-subscription"}, History: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "a", Provider: "DeepSeek", Usage: &protocol.UsageData{InputTokens: 27_000, OutputTokens: 400}},
		{Role: "assistant", Content: "b", Provider: "claude-subscription", Usage: &protocol.UsageData{InputTokens: 483_000, OutputTokens: 2_000}},
	}}
	if err := store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	ag := NewAgent(nil, nil, "DeepSeek", nil, nil, nil, store, NewContextManager(64000, 4000), nil)
	if err := ag.LoadSession("s1"); err != nil {
		t.Fatal(err)
	}
	if h := ag.history; h[1].UsageIsAggregate || !h[2].UsageIsAggregate {
		t.Fatalf("aggregate flags: %v %v", h[1].UsageIsAggregate, h[2].UsageIsAggregate)
	}
	if used, _ := ag.contextManager.contextTokens(ag.history, ag.history, 0); used > 30_000 {
		t.Fatalf("context = %d, want the DeepSeek reply's ~27K", used)
	}
}
