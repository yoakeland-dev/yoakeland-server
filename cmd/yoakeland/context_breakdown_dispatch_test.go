package main

import (
	"encoding/json"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// The ring (session_history.context, turn_completed.context) and the panel
// (context_breakdown) are two answers to one question; they must agree, and a
// chat nobody has opened since the server started still answers.
func TestServeConnection_ContextBreakdownAgreesWithSessionHistory(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SessStore.Save(&agent.SessionState{
		SessionID: "session-ctx", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		History: []agent.Message{
			{ID: "m1", Role: "user", Content: "hi"},
			{ID: "m2", Role: "assistant", Content: "hello", Model: "m", UsageIsAggregate: true,
				Usage: &protocol.UsageData{InputTokens: 900_000, ContextTokens: 5000, WindowTokens: 200_000}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	envs := runDispatchWaiting(t, deps,
		envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "session-ctx"}),
		envelopeJSON(t, protocol.EventGetContextBreakdown, protocol.GetContextBreakdownData{SessionID: "session-ctx", RequestID: "rq-1"}),
	)
	var hist protocol.SessionHistoryData
	var bd protocol.ContextBreakdownData
	var sawHist, sawBd bool
	for _, e := range envs {
		switch e.Type {
		case protocol.EventSessionHistory:
			sawHist = json.Unmarshal(e.Data, &hist) == nil
		case protocol.EventContextBreakdown:
			sawBd = json.Unmarshal(e.Data, &bd) == nil
		}
	}
	if !sawHist || !sawBd {
		t.Fatalf("expected session_history and context_breakdown, got %+v", envs)
	}
	if hist.Context == nil || hist.Context.EstimatedTokens != 5000 || hist.Context.WindowTokens != 200_000 || hist.Context.Source != "provider" {
		t.Fatalf("session_history.context = %+v", hist.Context)
	}
	if bd.RequestID != "rq-1" || bd.SessionID != "session-ctx" {
		t.Fatalf("reply not addressed to the request: %+v", bd)
	}
	if bd.TotalTokens != hist.Context.EstimatedTokens || bd.WindowTokens != hist.Context.WindowTokens || bd.Source != hist.Context.Source {
		t.Fatalf("the panel %+v disagrees with the ring %+v", bd, hist.Context)
	}
	used, spare := 0, 0
	for _, c := range bd.Categories {
		if c.Key == protocol.CtxFree || c.Key == protocol.CtxBuffer {
			spare += c.Tokens
		} else {
			used += c.Tokens
		}
	}
	if used != bd.TotalTokens || used+spare != bd.WindowTokens {
		t.Fatalf("categories add up to %d used + %d spare, want %d in %d: %+v", used, spare, bd.TotalTokens, bd.WindowTokens, bd.Categories)
	}
}

func TestServeConnection_ContextBreakdownNeedsASession(t *testing.T) {
	deps := newTestDeps(t)
	envs := runDispatchWaiting(t, deps, envelopeJSON(t, protocol.EventGetContextBreakdown, protocol.GetContextBreakdownData{}))
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error, got %+v", envs)
	}
}
