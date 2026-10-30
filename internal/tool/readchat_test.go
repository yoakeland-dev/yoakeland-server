package tool_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"yoakeland/internal/tool"
)

type fakeChats map[string][]tool.ChatTurn

func (f fakeChats) ReadChat(id string) (string, []tool.ChatTurn, error) {
	t, ok := f[id]
	if !ok {
		return "", nil, errors.New("missing")
	}
	return "Fix login", t, nil
}

func TestReadChatTool(t *testing.T) {
	rc := tool.NewReadChatTool(fakeChats{"a": {{"user", "why 500?"}, {"assistant", "the token expired"}}})
	ctx := context.Background()
	out, err := rc.Execute(ctx, tool.ExecutionContext{SessionID: "b"}, []byte(`{"chat":"chat:a"}`))
	if err != nil || !strings.Contains(out, "[1] user: why 500?") || !strings.Contains(out, "[2] assistant: the token expired") {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := rc.Execute(ctx, tool.ExecutionContext{SessionID: "a"}, []byte(`{"chat":"a"}`)); err == nil {
		t.Fatal("reading the current chat should be refused")
	}
	if _, err := rc.Execute(ctx, tool.ExecutionContext{SessionID: "b"}, []byte(`{"chat":"zzz"}`)); err == nil {
		t.Fatal("an unknown chat should fail")
	}
}

func TestReadChatToolKeepsHeadAndTail(t *testing.T) {
	var turns []tool.ChatTurn
	for i := 0; i < 20; i++ {
		turns = append(turns, tool.ChatTurn{Role: "user", Text: strings.Repeat("x", 5000) + string(rune('A'+i))})
	}
	out, err := tool.NewReadChatTool(fakeChats{"a": turns}).Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"chat":"a"}`))
	if err != nil || !strings.Contains(out, "xA\n") || !strings.Contains(out, "xT") || !strings.Contains(out, "left out: read them with from/to") || len(out) > 50000 {
		t.Fatalf("bad truncation (len %d): %v", len(out), err)
	}
}

func TestReadChatToolNarrowsTheRead(t *testing.T) {
	var turns []tool.ChatTurn
	for i := 0; i < 30; i++ {
		turns = append(turns, tool.ChatTurn{Role: "user", Text: "filler message number " + string(rune('a'+i%26))})
	}
	turns[17].Text = "the websocket reconnect loop never backs off, see ReconnectDelay"
	rc := tool.NewReadChatTool(fakeChats{"a": turns})
	run := func(args string) string {
		out, err := rc.Execute(context.Background(), tool.ExecutionContext{}, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		return out
	}
	if out := run(`{"chat":"a","query":"reconnectdelay websocket"}`); !strings.Contains(out, "[18] user: ") || strings.Contains(out, "[1] ") {
		t.Fatalf("query should find only message 18: %q", out)
	}
	if out := run(`{"chat":"a","query":"nothing like this"}`); !strings.Contains(out, "no message") {
		t.Fatalf("a miss should say so: %q", out)
	}
	if out := run(`{"chat":"a","from":17,"to":19}`); !strings.Contains(out, "[17] ") || !strings.Contains(out, "[19] ") || strings.Contains(out, "[16] ") || strings.Contains(out, "[20] ") {
		t.Fatalf("from/to range wrong: %q", out)
	}
	if out := run(`{"chat":"a","last":2}`); !strings.Contains(out, "[29] ") || !strings.Contains(out, "[30] ") || strings.Contains(out, "[28] ") {
		t.Fatalf("last 2 wrong: %q", out)
	}
	one := tool.NewReadChatTool(fakeChats{"a": {{"user", strings.Repeat("y", 90000)}}})
	if out, err := one.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"chat":"a"}`)); err != nil || len(out) > 50000 {
		t.Fatalf("one huge message should be cut (len %d, %v)", len(out), err)
	}
}
