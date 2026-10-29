package agent_test

// A turn cancelled while its provider stream has already emitted tool calls
// leaves those calls in the persisted history without any tool result: on the
// next reload every client renders them as still running, and the next turn
// sends a dangling tool call to the provider. The turn's exit must pair every
// call it recorded that never ran (or never finished) with a tool result.

import (
	"context"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

type cancelToolProvider struct {
	started     chan struct{}
	startedOnce sync.Once
	followup    chan []agent.Message
}

func (p *cancelToolProvider) StreamTurn(ctx context.Context, history []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	if p.followup != nil { // second turn: hand back the history and finish cleanly
		p.followup <- history
		cChan := make(chan agent.ModelStreamChunk, 1)
		eChan := make(chan error, 1)
		cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "Done."}
		close(cChan)
		close(eChan)
		return cChan, eChan
	}
	p.startedOnce.Do(func() { close(p.started) })
	cChan := make(chan agent.ModelStreamChunk, 3)
	eChan := make(chan error, 1)
	cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "Working on it."}
	cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "call-cancel-me", Name: "bash", Args: []byte(`{"argv":["sleep","30"]}`)}}
	go func() {
		<-ctx.Done()
		close(cChan)
		close(eChan)
	}()
	return cChan, eChan
}

func blockingChannels(ctx context.Context) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk)
	eChan := make(chan error)
	go func() {
		<-ctx.Done()
		close(cChan)
		close(eChan)
	}()
	return cChan, eChan
}

// Cancelling mid-stream must not leave the streamed tool call without a
// result, in the events the client saw or in the persisted history.
func TestCancelledTurn_PairsToolCallsWithResults(t *testing.T) {
	p := &cancelToolProvider{started: make(chan struct{})}
	ag, sessStore, outBuf := testAgent(t, p)
	_ = outBuf

	go func() {
		<-p.started
		time.Sleep(200 * time.Millisecond) // let the tool-call chunk land in history
		ag.CancelTurn("turn-1")
	}()

	if err := ag.RunTurn(context.Background(), "session-cancel", "turn-1", "go"); err == nil {
		t.Fatalf("expected the cancelled turn to report an error")
	}

	state, err := sessStore.Load("session-cancel")
	if err != nil {
		t.Fatalf("no persisted session: %v", err)
	}
	var answered bool
	var calls []string
	for _, m := range state.History {
		for _, c := range m.ToolCalls {
			calls = append(calls, c.ID)
		}
	}
	seen := map[string]bool{}
	for _, m := range state.History {
		if m.Role == "tool" && m.ToolCallID != "" {
			seen[m.ToolCallID] = true
			if m.ToolCallID == "call-cancel-me" {
				answered = true
			}
		}
	}
	for _, id := range calls {
		if !seen[id] {
			t.Fatalf("tool call %s left without a result in the persisted history", id)
		}
	}
	if !answered {
		t.Fatalf("cancelled turn's streamed tool call never got a tool result")
	}

	// The next turn must send a history with no dangling calls, and complete.
	p.followup = make(chan []agent.Message, 1)
	done := make(chan error, 1)
	go func() { done <- ag.RunTurn(context.Background(), "session-cancel", "turn-2", "again") }()
	select {
	case hist := <-p.followup:
		for _, m := range hist {
			for _, c := range m.ToolCalls {
				if !seen[c.ID] {
					t.Fatalf("follow-up turn's context carries tool call %s with no result", c.ID)
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("follow-up turn never ran its provider")
	}
	if err := <-done; err != nil {
		t.Fatalf("follow-up turn failed: %v", err)
	}
}
