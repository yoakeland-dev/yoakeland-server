package agent

import "testing"

// TestPrepareHistory_SummarizesDiscardedContent guards that compaction keeps
// a real trace of what happened in the discarded middle of a long session,
// not just a bare count — a bug fixed here (CompactSummary existed but was
// never wired into PrepareHistory, so the model lost that work outright).
func TestPrepareHistory_SummarizesDiscardedContent(t *testing.T) {
	cm := NewContextManager(10, 4000) // tiny budget forces compaction

	// PrepareHistory keeps history[0] and the last 4 messages verbatim; only
	// the messages strictly in between (index 1 here) are compacted away.
	history := []Message{
		{Role: "user", Content: "please refactor the payment module"},
		{Role: "assistant", Content: "renamed calculateTotal to computeTotal across the codebase"},
		{Role: "assistant", Content: "kept turn one"},
		{Role: "assistant", Content: "kept turn two"},
		{Role: "assistant", Content: "kept turn three"},
		{Role: "user", Content: "now add tests"},
	}

	compacted := cm.PrepareHistory(history)

	if len(compacted) != 6 {
		t.Fatalf("expected first message + summary + last 4, got %d messages: %+v", len(compacted), compacted)
	}
	if compacted[0].Content != history[0].Content {
		t.Fatalf("expected the first user message preserved verbatim, got %q", compacted[0].Content)
	}
	summary := compacted[1]
	if summary.Role != "system" {
		t.Fatalf("expected the second message to be the compaction summary, got role %q", summary.Role)
	}
	if !contains(summary.Content, "renamed calculateTotal to computeTotal") {
		t.Fatalf("expected the summary to retain a trace of the discarded turn's content, got: %q", summary.Content)
	}
}

// TestPrepareHistory_KeepsToolCallsPairedWithResults guards against slicing
// the last-4-messages tail in the middle of an assistant tool_calls message
// and its tool results: Anthropic and OpenAI both reject a request where a
// tool result's originating tool_calls message is missing (400 "tool_use
// ids were found without tool_result blocks" / "'tool' message must be a
// response to a preceding message with 'tool_calls'"), which is exactly
// what a naive last-4 cut produces once a tool-heavy turn runs long enough.
func TestPrepareHistory_KeepsToolCallsPairedWithResults(t *testing.T) {
	cm := NewContextManager(10, 4000) // tiny budget forces compaction

	history := []Message{
		{Role: "user", Content: "long task"},
		{Role: "assistant", Content: "step one"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "callA"}, {ID: "callB"}, {ID: "callC"}}},
		{Role: "tool", ToolCallID: "callA", Content: "result A"},
		{Role: "tool", ToolCallID: "callB", Content: "result B"},
		{Role: "tool", ToolCallID: "callC", Content: "result C"},
		{Role: "assistant", Content: "done"},
	}
	// A naive history[len(history)-4:] slice starts at index 3, a "tool"
	// message whose owning assistant (index 2) would be discarded.

	compacted := cm.PrepareHistory(history)

	toolCallIDs := map[string]bool{}
	for _, m := range compacted {
		for _, tc := range m.ToolCalls {
			toolCallIDs[tc.ID] = true
		}
	}
	for _, m := range compacted {
		if m.Role == "tool" && !toolCallIDs[m.ToolCallID] {
			t.Fatalf("tool result %q retained without its originating tool_calls message: %+v", m.ToolCallID, compacted)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
