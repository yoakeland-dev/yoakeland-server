package tool

import (
	"context"
	"fmt"
	"yoakeland/internal/protocol"
)

// TodoStore persists a session's todo list.
type TodoStore interface {
	SaveTodos(sessionID string, list protocol.TodoListData) error
}

const TodoToolName = "todo_write"

// TodoWriteTool replaces the session's todo list and pushes todo_list. It
// changes nothing but that list, so it needs no permission.
type TodoWriteTool struct{ store TodoStore }

func NewTodoWriteTool(store TodoStore) *TodoWriteTool { return &TodoWriteTool{store: store} }

func (t *TodoWriteTool) Name() string { return TodoToolName }

func (t *TodoWriteTool) Description() string {
	return "Replace your todo list for this task (shown to the user). Send the full list every time; keep one item in_progress."
}

func (t *TodoWriteTool) RequiresPermission(_ []byte) bool { return false }

func (t *TodoWriteTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	items, err := protocol.ParseTodoWrite(argsJSON)
	if err != nil {
		return "", err
	}
	if !protocol.ValidSessionID(execCtx.SessionID) {
		return "", fmt.Errorf("todo_write requires a valid session id")
	}
	list := protocol.NewTodoList(execCtx.SessionID, items)
	if t.store != nil {
		if err := t.store.SaveTodos(execCtx.SessionID, list); err != nil {
			return "", fmt.Errorf("saving todos: %w", err)
		}
	}
	if execCtx.Stream != nil {
		_ = execCtx.Stream.SendEvent(protocol.EventTodoList, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, list)
	}
	return fmt.Sprintf("todo list updated (%d items)", len(items)), nil
}

func (t *TodoWriteTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content":    map[string]any{"type": "string", "description": "What to do, imperative (\"Run the tests\")."},
							"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							"activeForm": map[string]any{"type": "string", "description": "Present-continuous form shown while in progress (\"Running the tests\")."},
						},
						"required": []string{"content", "status"},
					},
				},
			},
			"required": []string{"todos"},
		},
	}
}
