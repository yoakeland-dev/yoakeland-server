package agent

import (
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

func TestNewNativeTurn_NoteOnlyForNonClaude(t *testing.T) {
	a := &Agent{}
	hist := []Message{{Role: "user", Content: "hi"}}
	if p := a.newNativeTurn("s", "t", "p", hist, false, true).FreshPrompt; !strings.Contains(p, "artifact") || !strings.HasSuffix(p, "hi") {
		t.Fatalf("note missing from prompt: %q", p)
	}
	if p := a.newNativeTurn("s", "t", "p", hist, false, false).FreshPrompt; p != "hi" {
		t.Fatalf("claude prompt must stay bare: %q", p)
	}
}

func TestIsTodoWrite(t *testing.T) {
	for _, c := range []struct {
		tc   ToolCall
		want bool
	}{
		{ToolCall{Name: "TodoWrite"}, true},
		{ToolCall{Name: "mcpToolCall", NativeToolInfo: protocol.NativeToolInfo{Native: true, Title: "yoake-browser/todo_write"}}, true},
		{ToolCall{Name: "other", NativeToolInfo: protocol.NativeToolInfo{Native: true, Title: "yoake-browser_todo_write"}}, true},
		{ToolCall{Name: "other", NativeToolInfo: protocol.NativeToolInfo{Native: true, Title: "other-server_todo_write"}}, false},
	} {
		if got := isTodoWrite(&c.tc); got != c.want {
			t.Errorf("%+v = %v, want %v", c.tc, got, c.want)
		}
	}
}
