package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// TestServeConnection_EditMessageAndSwitchBranchRoundTrip exercises the wire
// path end to end (not just the internal agent package): a prompt, an edit
// of it, and a switch_branch back to the original — confirming the events
// dispatch correctly, session_history reports message IDs and AlternateIDs,
// and switching branches doesn't touch the model (the fake server would
// otherwise be hit a third time).
func TestServeConnection_EditMessageAndSwitchBranchRoundTrip(t *testing.T) {
	deps := newTestDeps(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		text := "first reply"
		if n > 1 {
			text = "second reply"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "test-anthropic", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic",
		APIKey: "sk-ant-test", BaseURL: srv.URL,
	})
	setLine := envelopeJSONWithSession(t, protocol.EventSetProvider, "session-edit", protocol.SetProviderData{
		ProviderName: "test-anthropic", Model: "claude-sonnet-5",
	})
	noTitleGen := false
	promptLine := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "hi", SessionID: "session-edit", AutoGenerateTitle: &noTitleGen,
	})
	runDispatchWaiting(t, deps, registerLine, setLine, promptLine)

	history := func() protocol.SessionHistoryData {
		t.Helper()
		envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "session-edit"}))
		if len(envs) != 1 || envs[0].Type != protocol.EventSessionHistory {
			t.Fatalf("expected one session_history envelope, got %+v", envs)
		}
		var data protocol.SessionHistoryData
		if err := json.Unmarshal(envs[0].Data, &data); err != nil {
			t.Fatalf("unmarshaling session_history: %v", err)
		}
		return data
	}

	initial := history()
	if len(initial.Messages) != 2 || initial.Messages[1].Content != "first reply" {
		t.Fatalf("unexpected initial history: %+v", initial.Messages)
	}
	origUserID := initial.Messages[0].ID
	if origUserID == "" {
		t.Fatalf("expected the user message to carry an id: %+v", initial.Messages[0])
	}

	editLine := envelopeJSON(t, protocol.EventEditMessage, protocol.EditMessageData{
		SessionID: "session-edit", MessageID: origUserID, Content: "hi again", AutoGenerateTitle: &noTitleGen,
	})
	runDispatchWaiting(t, deps, editLine)

	if calls != 2 {
		t.Fatalf("expected edit_message to trigger exactly one more model call, got %d total", calls)
	}

	edited := history()
	if len(edited.Messages) != 2 || edited.Messages[0].Content != "hi again" || edited.Messages[1].Content != "second reply" {
		t.Fatalf("unexpected history after edit: %+v", edited.Messages)
	}
	newUserID := edited.Messages[0].ID
	if newUserID == origUserID {
		t.Fatalf("expected the edited message to get a fresh id")
	}
	if len(edited.Messages[0].AlternateIDs) != 1 || edited.Messages[0].AlternateIDs[0] != origUserID {
		t.Fatalf("expected the original message listed as an alternate, got %+v", edited.Messages[0].AlternateIDs)
	}

	switchLine := envelopeJSON(t, protocol.EventSwitchBranch, protocol.SwitchBranchData{
		SessionID: "session-edit", MessageID: origUserID,
	})
	envs := runDispatch(t, deps, switchLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionHistory {
		t.Fatalf("expected switch_branch to respond with session_history, got %+v", envs)
	}
	var switched protocol.SessionHistoryData
	if err := json.Unmarshal(envs[0].Data, &switched); err != nil {
		t.Fatalf("unmarshaling session_history: %v", err)
	}
	if len(switched.Messages) != 2 || switched.Messages[0].Content != "hi" || switched.Messages[1].Content != "first reply" {
		t.Fatalf("switch_branch didn't restore the original branch: %+v", switched.Messages)
	}
	if len(switched.Messages[0].AlternateIDs) != 1 || switched.Messages[0].AlternateIDs[0] != newUserID {
		t.Fatalf("expected the edited branch listed as an alternate after switching away from it, got %+v", switched.Messages[0].AlternateIDs)
	}

	if calls != 2 {
		t.Fatalf("switch_branch must never call the model — expected 2 total calls, got %d", calls)
	}
}

// TestServeConnection_TurnCompletedCarriesRealMessageIDs is the wire-level
// guard for the production "message not found" bug: user_prompt's own
// turn_completed must carry the real server-assigned ids for the messages
// it just created, so a client never has to guess an id (or fall back to a
// racy get_session_history round trip) before it can edit_message/
// retry_message the turn it just watched stream in.
func TestServeConnection_TurnCompletedCarriesRealMessageIDs(t *testing.T) {
	deps := newTestDeps(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi there\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "test-anthropic", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic",
		APIKey: "sk-ant-test", BaseURL: srv.URL,
	})
	setLine := envelopeJSONWithSession(t, protocol.EventSetProvider, "session-turn-ids", protocol.SetProviderData{
		ProviderName: "test-anthropic", Model: "claude-sonnet-5",
	})
	noTitleGen := false
	promptLine := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "hi", SessionID: "session-turn-ids", AutoGenerateTitle: &noTitleGen,
	})
	envs := runDispatchWaiting(t, deps, registerLine, setLine, promptLine)

	var completed *protocol.TurnCompletedData
	for _, env := range envs {
		if env.Type != protocol.EventTurnCompleted {
			continue
		}
		var data protocol.TurnCompletedData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("malformed turn_completed data: %v", err)
		}
		completed = &data
	}
	if completed == nil {
		t.Fatalf("expected a turn_completed event, got %+v", envs)
	}
	if completed.UserMessageID == "" {
		t.Fatalf("expected turn_completed.userMessageId to be set, got %+v", completed)
	}
	if completed.MessageID == "" {
		t.Fatalf("expected turn_completed.messageId to be set, got %+v", completed)
	}

	historyEnvs := runDispatch(t, deps, envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "session-turn-ids"}))
	var history protocol.SessionHistoryData
	if err := json.Unmarshal(historyEnvs[0].Data, &history); err != nil {
		t.Fatalf("unmarshaling session_history: %v", err)
	}
	if len(history.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %+v", history.Messages)
	}
	if completed.UserMessageID != history.Messages[0].ID {
		t.Fatalf("turn_completed.userMessageId %q doesn't match session_history's %q", completed.UserMessageID, history.Messages[0].ID)
	}
	if completed.MessageID != history.Messages[1].ID {
		t.Fatalf("turn_completed.messageId %q doesn't match session_history's %q", completed.MessageID, history.Messages[1].ID)
	}
}
