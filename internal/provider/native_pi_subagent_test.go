package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"yoakeland/internal/agent"
)

// A Pi subagent runs inside its "subagent" tool call. Its own tool calls and results arrive in
// that call's updates and final result, and are tagged with the call as their parent.
func TestPiSubagentNestsInnerCallsUnderParent(t *testing.T) {
	var got []string
	turn := &piTurn{
		s:     &piSession{flavor: piFlavorPi},
		tools: map[string]string{},
		emit: func(c agent.ModelStreamChunk) {
			switch c.Kind {
			case agent.StreamChunkToolCall:
				got = append(got, fmt.Sprintf("call %s %s parent=%q", c.ToolCall.ID, c.ToolCall.Name, c.ToolCall.ParentToolCallID))
			case agent.StreamChunkToolResult:
				got = append(got, fmt.Sprintf("result %s %s parent=%q output=%q", c.ToolResult.ToolCallID, c.ToolResult.Name, c.ToolResult.ParentToolCallID, c.ToolResult.Output))
			}
		},
	}
	feed := func(line string) {
		var ln piLine
		if err := json.Unmarshal([]byte(line), &ln); err != nil {
			t.Fatal(err)
		}
		turn.handle(ln)
	}

	feed(`{"type":"tool_execution_start","toolCallId":"call_sub","toolName":"subagent","args":{"agent":"scout","task":"map the repo"}}`)
	// The update repeats the transcript so far, so it is fed twice.
	update := `{"type":"tool_execution_update","toolCallId":"call_sub","toolName":"subagent","partialResult":{"content":[{"type":"text","text":"(running...)"}],"details":{"mode":"single","results":[{"agent":"scout","task":"map the repo","messages":[
		{"role":"assistant","content":[{"type":"toolCall","id":"inner_1","name":"read","arguments":{"path":"README.md"}}]},
		{"role":"toolResult","toolCallId":"inner_1","toolName":"read","isError":false,"content":[{"type":"text","text":"# hi"}]}
	]}]}}}`
	feed(update)
	feed(update)
	feed(`{"type":"tool_execution_end","toolCallId":"call_sub","toolName":"subagent","isError":false,"result":{"content":[{"type":"text","text":"done"}],"details":{"mode":"single","results":[{"agent":"scout","task":"map the repo","exitCode":0,"stopReason":"stop","messages":[
		{"role":"assistant","content":[{"type":"toolCall","id":"inner_1","name":"read","arguments":{"path":"README.md"}}]},
		{"role":"toolResult","toolCallId":"inner_1","toolName":"read","isError":false,"content":[{"type":"text","text":"# hi"}]},
		{"role":"assistant","content":[{"type":"toolCall","id":"inner_2","name":"bash","arguments":{"command":"ls"}}]},
		{"role":"toolResult","toolCallId":"inner_2","toolName":"bash","isError":false,"content":[{"type":"text","text":"README.md"}]},
		{"role":"assistant","content":[{"type":"text","text":"repo has one file"}]}
	]}]}}}`)

	want := []string{
		`call call_sub subagent parent=""`,
		`call inner_1 read parent="call_sub"`,
		`result inner_1 read parent="call_sub" output="# hi"`,
		`call inner_2 bash parent="call_sub"`,
		`result inner_2 bash parent="call_sub" output="README.md"`,
		`result call_sub subagent parent="" output="done"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chunks:\n got %q\nwant %q", got, want)
	}
}
