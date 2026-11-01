package tool

import (
	"context"
	"strings"
	"sync"
	"testing"

	"yoakeland/internal/protocol"
)

type memTodos struct {
	mu sync.Mutex
	l  map[string]protocol.TodoListData
}

func (m *memTodos) SaveTodos(id string, l protocol.TodoListData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.l[id] = l
	return nil
}

type evCap struct{ evs []protocol.TodoListData }

func (e *evCap) SendEvent(t protocol.EventType, _, _, _ string, p any) error {
	if t == protocol.EventTodoList {
		e.evs = append(e.evs, p.(protocol.TodoListData))
	}
	return nil
}

func TestTodoWriteReplacesAndEmits(t *testing.T) {
	st := &memTodos{l: map[string]protocol.TodoListData{}}
	tl := NewTodoWriteTool(st)
	ev := &evCap{}
	ctx := ExecutionContext{SessionID: "s1", Stream: ev}
	run := func(js string) error { _, err := tl.Execute(context.Background(), ctx, []byte(js)); return err }
	if err := run(`{"todos":[{"content":"a","status":"in_progress","activeForm":"Doing a"},{"content":"b","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := run(`{"todos":[{"content":"c","status":"completed"}]}`); err != nil {
		t.Fatal(err)
	}
	got := st.l["s1"]
	if len(got.Items) != 1 || got.Items[0].Content != "c" || got.Items[0].ID != "1" || got.UpdatedAtMs == 0 {
		t.Fatalf("not replaced: %+v", got)
	}
	if len(ev.evs) != 2 || len(ev.evs[0].Items) != 2 {
		t.Fatalf("events: %+v", ev.evs)
	}
	if err := run(`{"todos":[]}`); err != nil || len(st.l["s1"].Items) != 0 {
		t.Fatal("clearing failed", err)
	}
}

func TestTodoWriteValidation(t *testing.T) {
	tl := NewTodoWriteTool(nil)
	ctx := ExecutionContext{SessionID: "s1"}
	many := `{"todos":[` + strings.Repeat(`{"content":"x","status":"pending"},`, 50) + `{"content":"x","status":"pending"}]}`
	for name, js := range map[string]string{
		"bad status": `{"todos":[{"content":"a","status":"done"}]}`,
		"empty":      `{"todos":[{"content":" ","status":"pending"}]}`,
		"long":       `{"todos":[{"content":"` + strings.Repeat("x", 501) + `","status":"pending"}]}`,
		"too many":   many,
	} {
		if _, err := tl.Execute(context.Background(), ctx, []byte(js)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if tl.RequiresPermission(nil) {
		t.Error("should not need permission")
	}
}
