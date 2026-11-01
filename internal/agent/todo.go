package agent

import (
	"context"
	"strings"

	"yoakeland/internal/tool"
)

// recordTodos feeds a CLI agent's TodoWrite input (same shape as todo_write)
// through the todo_write tool: it stores the list and pushes todo_list.
func (a *Agent) recordTodos(sessionID, turnID string, tc *ToolCall) {
	t, ok := a.tools[tool.TodoToolName]
	// Claude Code's own TodoWrite, or the todo_write tool of this app's MCP server, which is what a CLI
	// agent running headless has (it has no TodoWrite there).
	if !ok || !isTodoWrite(tc) || tc.ParentToolCallID != "" {
		return
	}
	_, _ = t.Execute(context.Background(), tool.ExecutionContext{SessionID: sessionID, TurnID: turnID, ToolCallID: tc.ID, Stream: a.getStream()}, tc.Args)
}

// isTodoWrite: Claude's TodoWrite or this app's todo_write. Codex and opencode report an MCP call
// under a generic name, with the server and tool in the title ("yoake-browser/todo_write", "yoake-browser_todo_write").
func isTodoWrite(tc *ToolCall) bool {
	if tc.Name == "TodoWrite" || tc.Name == "mcp__yoake-browser__"+tool.TodoToolName {
		return true
	}
	return tc.Native && strings.HasPrefix(tc.Title, "yoake-browser") && strings.HasSuffix(tc.Title, tool.TodoToolName)
}
