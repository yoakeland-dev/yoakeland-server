package agent

import (
	"bytes"
	"context"
	"testing"

	"yoakeland/internal/protocol"
)

func TestDropEmptyAssistants(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Reasoning: "thinking only"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c"}}},
		{Role: "assistant", Content: "text"},
	}
	if got := dropEmptyAssistants(in); len(got) != 3 || got[1].ToolCalls == nil {
		t.Fatalf("got %+v", got)
	}
}

// AGT-8: a subagent is held to the cost its round's tally shows, siblings
// included, not just its own spend (which is never recorded in a usage store).
func TestLimitHit_SubagentUsesSharedTally(t *testing.T) {
	a := NewAgent(nil, nil, "", nil, nil, nil, nil, &ContextManager{}, nil)
	a.SetLimits(NewLimits(0, 1.0))
	a.tally = &spendTally{usd: 0.6}
	if kind, _ := a.limitHit(&turnLimits{}, 0.1); kind != limitNone {
		t.Fatalf("0.6 of a 1.0 cap should not stop it, got %v", kind)
	}
	a.tally.add(0.5) // a sibling's round
	if kind, _ := a.limitHit(&turnLimits{}, 0.1); kind != limitCost {
		t.Fatalf("the tally at 1.1 of a 1.0 cap should stop it, got %v", kind)
	}
}

// AGT-10: a cancel that lands after a turn takes the slot but before its
// generation loop starts still reaches the turn.
func TestLockTurn_CancelReachesTurnBeforeItStarts(t *testing.T) {
	a := NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nil, "", nil, nil, nil, nil, &ContextManager{}, nil)
	ctx, release, ok := a.lockTurn(context.Background(), "s", "t1")
	if !ok {
		t.Fatal("an idle agent should grant the turn slot")
	}
	defer release()
	a.CancelTurn("t1")
	if ctx.Err() == nil {
		t.Fatal("CancelTurn did not reach the turn's context")
	}
}
