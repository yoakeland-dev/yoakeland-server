package agent

import "testing"

func TestDropEmptyAssistants(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Reasoning: "thinking only"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c"}}},
		{Role: "assistant", Content: "text"},
	}
	if got := dropEmptyAssistants(in); len(got) != 3 || got[1].ToolCalls == nil {
		t.Fatalf("got %+v", got)
	}
}
