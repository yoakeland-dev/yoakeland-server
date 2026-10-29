package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/diff"
)

// A turn in which the main thread spawns a subagent and the subagent works.
// Each row is one notification and what the turn emits for it.
func TestCodexSubagentEventsNestUnderSpawn(t *testing.T) {
	var got []string
	s := &codexSession{thread: "main", child: map[string]string{}}
	tn := &codexTurn{s: s, changes: map[string][]*diff.FileDiff{}, done: make(chan json.RawMessage, 1), emit: func(c agent.ModelStreamChunk) {
		switch c.Kind {
		case agent.StreamChunkDelta:
			got = append(got, "delta:"+c.Delta)
		case agent.StreamChunkToolCall:
			got = append(got, "call:"+c.ToolCall.ID+"<"+c.ToolCall.ParentToolCallID)
		case agent.StreamChunkToolResult:
			got = append(got, "result:"+c.ToolResult.ToolCallID+"<"+c.ToolResult.ParentToolCallID)
		}
	}}
	rows := []struct{ name, method, params, want string }{
		{"main reply", "item/agentMessage/delta", `{"threadId":"main","delta":"hi"}`, "delta:hi"},
		{"spawn starts", "item/started", `{"threadId":"main","item":{"type":"collabAgentToolCall","id":"sp1","tool":"spawnAgent","prompt":"find the bug","receiverThreadIds":[]}}`, "call:sp1<"},
		{"spawn done names the child", "item/completed", `{"threadId":"main","item":{"type":"collabAgentToolCall","id":"sp1","tool":"spawnAgent","status":"completed","receiverThreadIds":["child"]}}`, "result:sp1<"},
		{"child command starts", "item/started", `{"threadId":"child","item":{"type":"commandExecution","id":"c1","command":"ls"}}`, "call:c1<sp1"},
		{"child command ends", "item/completed", `{"threadId":"child","item":{"type":"commandExecution","id":"c1","command":"ls","status":"completed","aggregatedOutput":"a.txt","exitCode":0}}`, "result:c1<sp1"},
		{"child message is not the reply", "item/agentMessage/delta", `{"threadId":"child","delta":"sub"}`, ""},
		{"child turn end is not ours", "turn/completed", `{"threadId":"child","turn":{"id":"t-child","status":"completed"}}`, ""},
		{"main usage sets context", "thread/tokenUsage/updated", `{"threadId":"main","tokenUsage":{"last":{"inputTokens":100,"outputTokens":10},"modelContextWindow":258400}}`, ""},
		{"child usage counts as spend only", "thread/tokenUsage/updated", `{"threadId":"child","tokenUsage":{"last":{"inputTokens":40,"outputTokens":2},"modelContextWindow":1000}}`, ""},
		{"subagent event before its spawn names it", "item/started", `{"threadId":"late","item":{"type":"commandExecution","id":"l1","command":"pwd"}}`, ""},
		{"spawn names the early thread, replays it", "item/completed", `{"threadId":"main","item":{"type":"collabAgentToolCall","id":"sp2","tool":"spawnAgent","status":"completed","receiverThreadIds":["late"]}}`, "result:sp2< call:l1<sp2"},
		{"main turn ends", "turn/completed", `{"threadId":"main","turn":{"id":"t-main","status":"completed"}}`, ""},
	}
	for _, r := range rows {
		got = nil
		tn.notify(r.method, json.RawMessage(r.params))
		if g := strings.Join(got, " "); g != r.want {
			t.Errorf("%s: emitted %q, want %q", r.name, g, r.want)
		}
	}
	// The child's turn end must not have ended ours.
	select {
	case raw := <-tn.done:
		if !strings.Contains(string(raw), "t-main") {
			t.Errorf("turn ended with %s, want the main turn", raw)
		}
	default:
		t.Error("main turn/completed did not end the turn")
	}
	if u := tn.usage; u.InputTokens != 140 || u.ContextTokens != 110 || u.WindowTokens != 258400 {
		t.Errorf("usage = %+v, want spend 140, context 110, window 258400", u)
	}
}
