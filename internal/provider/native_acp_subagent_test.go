package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"yoakeland/internal/agent"
)

// A Grok subagent's own updates arrive on its own sessionId, and its spawn
// call names that session in the output. Its tool calls are filed under the
// spawn call, its text is not shown, and its usage does not count.
func TestGrokSubagentNestsChildCallsUnderSpawn(t *testing.T) {
	var got []string
	a := &acpLive{sess: acpSession{SessionID: "root"}}
	turn := &acpTurn{
		a:     a,
		emit:  func(c agent.ModelStreamChunk) { got = append(got, describeChunk(c)) },
		sub:   acpSubagentHooks[CLIKindGrok],
		calls: map[string]*acpToolCall{}, done: map[string]bool{},
		kids: map[string]string{}, pending: map[string][]json.RawMessage{}, cost: -1,
	}
	a.turn = turn
	const child = "019f0220-e192-7c41-9e9d-b406bc3459c8" // the id the spawn names below
	feed := func(session, update string) {
		a.onNotify("session/update", json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"update":%s}`, session, update)))
	}

	feed("root", `{"sessionUpdate":"tool_call","toolCallId":"spawn-1","title":"Task","rawInput":{"description":"Explore server","prompt":"Audit","subagent_type":"generalPurpose"}}`)
	// The child's updates come before its spawn has named it: held, not shown.
	feed(child, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"I'll audit"}}`)
	feed(child, `{"sessionUpdate":"tool_call","toolCallId":"read-1","title":"Read","kind":"read","rawInput":{"path":"a.go"}}`)
	feed(child, `{"sessionUpdate":"usage_update","used":999,"size":1000}`)
	feed("root", `{"sessionUpdate":"tool_call_update","toolCallId":"spawn-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"Agent ID: 019f0220-e192-7c41-9e9d-b406bc3459c8 (can be resumed)"}}]}`)
	feed(child, `{"sessionUpdate":"tool_call_update","toolCallId":"read-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"package a"}}]}`)
	feed(child, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Audit done"}}`)
	feed("root", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}`)

	want := []string{
		`call spawn-1 Task parent=""`,
		`result spawn-1 Task parent="" output="Agent ID: 019f0220-e192-7c41-9e9d-b406bc3459c8 (can be resumed)"`,
		`call read-1 read parent="spawn-1"`,
		`result read-1 read parent="spawn-1" output="package a"`,
		`delta "done"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chunks:\n got %q\nwant %q", got, want)
	}
	if turn.used != 0 {
		t.Errorf("a subagent's usage counted toward the turn: used=%d", turn.used)
	}
}

// An opencode subagent's own updates arrive on its own sessionId, and its
// running tool_call_update names that session in rawOutput.metadata. Its
// tool calls are filed under the spawn call, its text is not shown.
func TestOpenCodeSubagentNestsChildCallsUnderSpawn(t *testing.T) {
	var got []string
	a := &acpLive{sess: acpSession{SessionID: "root"}}
	turn := &acpTurn{
		a:     a,
		emit:  func(c agent.ModelStreamChunk) { got = append(got, describeChunk(c)) },
		sub:   acpSubagentHooks[CLIKindOpenCode],
		calls: map[string]*acpToolCall{}, done: map[string]bool{},
		kids: map[string]string{}, pending: map[string][]json.RawMessage{}, cost: -1,
	}
	a.turn = turn
	const child = "ses_child"
	feed := func(session, update string) {
		a.onNotify("session/update", json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"update":%s}`, session, update)))
	}

	feed("root", `{"sessionUpdate":"tool_call","toolCallId":"spawn-1","title":"Explore server","kind":"task","rawInput":{"description":"Explore server","prompt":"Audit","subagent_type":"general-purpose"}}`)
	// While the task runs, its update names the child session; the child's
	// own updates stream on their own sessionId and are held until then.
	feed("root", `{"sessionUpdate":"tool_call_update","toolCallId":"spawn-1","status":"in_progress","rawOutput":{"metadata":{"sessionId":"ses_child","parentSessionId":"root"}}}`)
	feed(child, `{"sessionUpdate":"tool_call","toolCallId":"grep-1","title":"Grep","kind":"search","rawInput":{"pattern":"TODO"}}`)
	feed(child, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"found one"}}`)
	feed("root", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}`)

	want := []string{
		`call spawn-1 Task parent=""`,
		`call grep-1 search parent="spawn-1"`,
		`delta "done"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chunks:\n got %q\nwant %q", got, want)
	}
}

func describeChunk(c agent.ModelStreamChunk) string {
	switch c.Kind {
	case agent.StreamChunkToolCall:
		return fmt.Sprintf("call %s %s parent=%q", c.ToolCall.ID, c.ToolCall.Name, c.ToolCall.ParentToolCallID)
	case agent.StreamChunkToolResult:
		return fmt.Sprintf("result %s %s parent=%q output=%q", c.ToolResult.ToolCallID, c.ToolResult.Name, c.ToolResult.ParentToolCallID, c.ToolResult.Output)
	case agent.StreamChunkDelta:
		return fmt.Sprintf("delta %q", c.Delta)
	}
	return fmt.Sprintf("other %v", c.Kind)
}
