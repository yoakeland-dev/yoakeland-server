package agent_test

import (
	"context"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// TestAgent_AssistantMessageStampsProviderModelAndTiming guards the fix for
// the model footer disappearing on session reload: get_session_history
// replays whatever's on disk (project.go's toHistoryMessages), so if the
// stored Message itself doesn't carry Provider/Model/CreatedAt/GenerationMs/
// Usage, no amount of frontend plumbing can bring the footer back after a
// reload.
func TestAgent_AssistantMessageStampsProviderModelAndTiming(t *testing.T) {
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{Kind: agent.StreamChunkUsage, Usage: &protocol.UsageData{InputTokens: 1200, OutputTokens: 40, TotalTokens: 1240}},
				{Kind: agent.StreamChunkDelta, Delta: "hi there"},
			},
		},
	}
	ag, sessStore, _ := testAgent(t, provider)

	before := time.Now()
	if err := ag.RunTurn(context.Background(), "session-meta", "turn-1", "hello"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	state, err := sessStore.Load("session-meta")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("history = %+v, want [user, assistant]", state.History)
	}

	user, reply := state.History[0], state.History[1]
	if user.CreatedAt.Before(before) {
		t.Fatalf("user message CreatedAt = %v, want at/after %v", user.CreatedAt, before)
	}

	if reply.Provider != "test-provider" {
		t.Fatalf("assistant Provider = %q, want %q", reply.Provider, "test-provider")
	}
	if reply.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("assistant Model = %q, want the scripted provider's model", reply.Model)
	}
	if reply.CreatedAt.Before(before) {
		t.Fatalf("assistant CreatedAt = %v, want at/after %v", reply.CreatedAt, before)
	}
	if reply.GenerationMs < 0 {
		t.Fatalf("assistant GenerationMs = %d, want >= 0", reply.GenerationMs)
	}
	if reply.Usage == nil {
		t.Fatalf("assistant message has no Usage stamped")
	}
	if reply.Usage.InputTokens != 1200 || reply.Usage.OutputTokens != 40 {
		t.Fatalf("assistant Usage = %+v, want InputTokens=1200 OutputTokens=40", reply.Usage)
	}
	if reply.Usage.CostUSD <= 0 {
		t.Fatalf("assistant Usage.CostUSD = %v, want a positive estimate from the pricing table", reply.Usage.CostUSD)
	}
}
