package agent

import (
	"encoding/json"
	"testing"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type todoMem struct{ got *protocol.TodoListData }

func (m *todoMem) SaveTodos(_ string, l protocol.TodoListData) error { m.got = &l; return nil }

func TestRecordTodosParsesCLITodoWrite(t *testing.T) {
	m := &todoMem{}
	a := &Agent{stream: protocol.NewStreamSet(), tools: map[string]tool.Tool{tool.TodoToolName: tool.NewTodoWriteTool(m)}}
	args := json.RawMessage(`{"todos":[{"content":"Fix it","status":"in_progress","activeForm":"Fixing it"},{"content":"Test","status":"pending"}]}`)
	a.recordTodos("s1", "t1", &ToolCall{ID: "c1", Name: "TodoWrite", Args: args})
	if m.got == nil || len(m.got.Items) != 2 || m.got.Items[0].ActiveForm != "Fixing it" || m.got.Items[1].Status != "pending" {
		t.Fatalf("got %+v", m.got)
	}
	m.got = nil
	a.recordTodos("s1", "t1", &ToolCall{ID: "c2", Name: "Bash", Args: args})
	if m.got != nil {
		t.Fatal("non-TodoWrite recorded")
	}
}

func TestTodosPersistWithSession(t *testing.T) {
	store, err := NewDiskSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	list := protocol.NewTodoList("s1", []protocol.TodoItem{{ID: "1", Content: "a", Status: "pending"}})
	if _, err := store.Update("s1", func(s *SessionState) error { s.Todos = &list; return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load("s1")
	if err != nil || st.Todos == nil || len(st.Todos.Items) != 1 || st.Todos.UpdatedAtMs != list.UpdatedAtMs {
		t.Fatalf("round trip: %+v %v", st, err)
	}
}
