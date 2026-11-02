package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

// brokenArgs is what a provider that repeated or merged its tool-call
// fragments hands back: two JSON values where one is expected.
const brokenArgs = `{"path":"a.go"},{"path":"b.go"}`

// probeTool records the arguments it was given and fails on unparseable ones,
// the way every real tool does (SPEC.md §3.2).
type probeTool struct{ got []byte }

func (t *probeTool) Name() string                    { return "probe" }
func (t *probeTool) Description() string             { return "probe" }
func (t *probeTool) RequiresPermission([]byte) bool  { return false }
func (t *probeTool) Definition() tool.ToolDefinition { return tool.ToolDefinition{Name: "probe"} }
func (t *probeTool) Execute(_ context.Context, _ tool.ExecutionContext, args []byte) (string, error) {
	t.got = append([]byte(nil), args...)
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", errors.New("invalid arguments: " + err.Error())
	}
	return "path=" + in.Path, nil
}

// A tool call whose args aren't one JSON value must not cost the session: the
// turn still runs (the tool reports its own error) and the history that gets
// saved carries a stub instead of the unparseable bytes.
func TestRunTurn_UnparseableToolArgsStillSave(t *testing.T) {
	probe := &probeTool{}
	p := &scriptProvider{func(h []agent.Message, _ string) []agent.ModelStreamChunk {
		for _, m := range h {
			for _, tc := range m.ToolCalls {
				if tc.Name == "probe" {
					return nil // the second round: nothing left to say
				}
			}
		}
		return []agent.ModelStreamChunk{{
			Kind:     agent.StreamChunkToolCall,
			ToolCall: &agent.ToolCall{ID: "c1", Name: "probe", Args: json.RawMessage(brokenArgs)},
		}}
	}}
	ag, sessStore := newFixesAgent(t, p, probe)
	if err := ag.RunTurn(context.Background(), "s", "t1", "go"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	// The call itself ran against what the model sent, so the tool's own error
	// is the signal the user sees.
	if string(probe.got) != brokenArgs {
		t.Fatalf("the tool ran with %q, want the model's own bytes %q", probe.got, brokenArgs)
	}

	// What was stored is readable again: valid JSON, with the model's text kept.
	state, err := sessStore.Load("s")
	if err != nil {
		t.Fatalf("the session did not survive being saved: %v", err)
	}
	var stored *agent.ToolCall
	for i, m := range state.History {
		if m.Role == "tool" && !strings.Contains(m.Content, "invalid arguments") {
			t.Fatalf("tool result = %q, want the tool's own error", m.Content)
		}
		if len(m.ToolCalls) > 0 {
			stored = &state.History[i].ToolCalls[0]
		}
	}
	if stored == nil {
		t.Fatal("the tool call is missing from the saved history")
	}
	if !json.Valid(stored.Args) {
		t.Fatalf("saved args are not valid JSON: %q", stored.Args)
	}
	if !strings.Contains(string(stored.Args), "b.go") {
		t.Fatalf("saved args lost the model's text: %s", stored.Args)
	}
	if stored.ID != "c1" || stored.Name != "probe" {
		t.Fatalf("saved call = %+v, want the call itself unchanged", stored)
	}
}

// The store repairs what it is handed rather than failing the write, and does
// not reach into the caller's state to do it.
func TestSessionStore_RepairsUnmarshallableState(t *testing.T) {
	store, err := agent.NewDiskSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	poisoned := &agent.SessionState{
		SessionID: "s1",
		History: []agent.Message{
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "probe", Args: json.RawMessage(brokenArgs)}}},
			{Role: "assistant", Content: "hi", RawContent: json.RawMessage(`{"a":1},{"b":2}`)},
		},
	}
	if err := store.Save(poisoned); err != nil {
		t.Fatalf("save failed instead of repairing: %v", err)
	}
	if string(poisoned.History[0].ToolCalls[0].Args) != brokenArgs {
		t.Fatalf("the store modified the caller's history: %q", poisoned.History[0].ToolCalls[0].Args)
	}

	got, err := store.Load("s1")
	if err != nil {
		t.Fatalf("repaired session does not load: %v", err)
	}
	if a := got.History[0].ToolCalls[0].Args; !json.Valid(a) || !strings.Contains(string(a), "b.go") {
		t.Fatalf("saved args = %q, want a valid stub keeping the model's text", a)
	}
	if rc := got.History[1].RawContent; len(rc) != 0 {
		t.Fatalf("saved rawContent = %q, want it dropped", rc)
	}
	if got.History[1].Content != "hi" {
		t.Fatalf("the rest of the message was lost: %+v", got.History[1])
	}
}
