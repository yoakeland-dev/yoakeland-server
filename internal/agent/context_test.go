package agent

import (
	"strings"
	"testing"

	"yoakeland/internal/protocol"
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
	if got := cm.contextTokens(history, history, 0); got != 1100 {
		t.Fatalf("expected reported 1000 + 100 estimated, got %d", got)
	}
	history = append(history, Message{Role: "compaction", Compaction: &Compaction{Kind: CompactionClear}})
	if got := cm.contextTokens(history, history[:3], 50); got >= 1000 {
		t.Fatalf("expected a fresh estimate after compaction, got %d", got)
	}
}
