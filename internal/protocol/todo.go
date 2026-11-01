package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	MaxTodoItems = 50
	MaxTodoChars = 500
)

// TodoItem is one entry of a session's todo list (SPEC.md §3.2 todo_list).
type TodoItem struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed
	ActiveForm string `json:"activeForm,omitempty"`
}

// TodoListData is the todo_list push: the whole list, replaced on every change.
type TodoListData struct {
	SessionID   string     `json:"sessionId"`
	Items       []TodoItem `json:"items"`
	UpdatedAtMs int64      `json:"updatedAtMs"`
}

// ParseTodoWrite reads a todo_write / Claude CLI TodoWrite input
// ({"todos":[{content,status,activeForm?}]}) into a validated list. IDs are
// positional ("1", "2", ...).
func ParseTodoWrite(args []byte) ([]TodoItem, error) {
	var in struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(in.Todos) > MaxTodoItems {
		return nil, fmt.Errorf("too many todos (%d, max %d)", len(in.Todos), MaxTodoItems)
	}
	for i := range in.Todos {
		t := &in.Todos[i]
		t.Content = strings.TrimSpace(t.Content)
		t.ActiveForm = strings.TrimSpace(t.ActiveForm)
		if t.Content == "" {
			return nil, fmt.Errorf("todo %d has no content", i+1)
		}
		if len([]rune(t.Content)) > MaxTodoChars || len([]rune(t.ActiveForm)) > MaxTodoChars {
			return nil, fmt.Errorf("todo %d is longer than %d characters", i+1, MaxTodoChars)
		}
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return nil, fmt.Errorf("todo %d has invalid status %q (pending, in_progress or completed)", i+1, t.Status)
		}
		t.ID = strconv.Itoa(i + 1)
	}
	return in.Todos, nil
}

// NewTodoList stamps a list for sessionID with the current time.
func NewTodoList(sessionID string, items []TodoItem) TodoListData {
	if items == nil {
		items = []TodoItem{}
	}
	return TodoListData{SessionID: sessionID, Items: items, UpdatedAtMs: time.Now().UnixMilli()}
}
