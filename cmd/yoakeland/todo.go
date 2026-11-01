package main

import (
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// todoStore keeps a session's todo list in its session file.
type todoStore struct{ s agent.SessionStore }

func (t todoStore) SaveTodos(sessionID string, list protocol.TodoListData) error {
	_, err := t.s.Update(sessionID, func(st *agent.SessionState) error {
		st.Todos = &list
		return nil
	})
	return err
}

// sendTodoList re-sends a session's saved todo list to one connection (session open).
func sendTodoList(to protocol.EventSender, sessionID string, st *agent.SessionState) {
	if st != nil && st.Todos != nil && len(st.Todos.Items) > 0 {
		l := *st.Todos
		l.SessionID = sessionID
		_ = to.SendEvent(protocol.EventTodoList, sessionID, "", "", l)
	}
}
