package provider

import (
	"encoding/json"
	"testing"
)

func TestClaudeCompactionLines(t *testing.T) {
	var ln claudeLine
	raw := `{"type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context.\n\nSummary:\n1. Goal: x\n"}}`
	if err := json.Unmarshal([]byte(raw), &ln); err != nil {
		t.Fatalf("a string content must parse: %v", err)
	}
	if len(ln.Message.Content) != 1 || ln.Message.Content[0].Type != "text" {
		t.Fatalf("content: %+v", ln.Message.Content)
	}
	if got := claudeSummaryOf(ln.Message.Content[0].Text); got != "1. Goal: x" {
		t.Fatalf("summary: %q", got)
	}
	if err := json.Unmarshal([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a"}]}}`), &ln); err != nil || ln.Message.Content[0].ToolUseID != "a" {
		t.Fatalf("block content still parses: %v %+v", err, ln.Message.Content)
	}
}
