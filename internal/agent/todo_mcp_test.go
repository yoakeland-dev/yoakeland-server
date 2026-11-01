package agent

import (
	"testing"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// A headless CLI agent has no TodoWrite; it calls the MCP server's todo_write, and the agent
// records that like the other.
func TestRecordTodosAcceptsTheMCPTool(t *testing.T) {
	for _, name := range []string{"TodoWrite", "mcp__yoake-browser__todo_write"} {
		store := &todoMem{}
		a := &Agent{stream: protocol.NewStreamSet(), tools: map[string]tool.Tool{tool.TodoToolName: tool.NewTodoWriteTool(store)}}
		a.recordTodos("s1", "t1", &ToolCall{ID: "c1", Name: name, Args: []byte(`{"todos":[{"content":"x","status":"in_progress"}]}`)})
		if store.got == nil {
			t.Errorf("%s: saved nothing", name)
		}
	}
	store := &todoMem{}
	a := &Agent{stream: protocol.NewStreamSet(), tools: map[string]tool.Tool{tool.TodoToolName: tool.NewTodoWriteTool(store)}}
	a.recordTodos("s1", "t1", &ToolCall{ID: "c2", Name: "mcp__other__todo_write", Args: []byte(`{"todos":[]}`)})
	if store.got != nil {
		t.Error("another server's todo_write was recorded")
	}
}
