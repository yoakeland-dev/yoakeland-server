package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// A prompt's declared sender has to reach the stored message and come back out
// through session_history, since that's the only path a repainted transcript
// has for an avatar: it exists nowhere but the sending client's own settings.
func TestServeConnection_PromptAuthorRoundTripsThroughHistory(t *testing.T) {
	deps := newTestDeps(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "test-anthropic", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic",
		APIKey: "sk-ant-test", BaseURL: srv.URL,
	})
	setLine := envelopeJSONWithSession(t, protocol.EventSetProvider, "session-author", protocol.SetProviderData{
		ProviderName: "test-anthropic", Model: "claude-sonnet-5",
	})
	promptLine := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "who am i", SessionID: "session-author",
		AuthorName: "aveline", AuthorAvatar: "🐣",
	})
	runDispatchWaiting(t, deps, registerLine, setLine, promptLine)

	// Read it back the way a client that just reconnected does.
	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventGetSessionHistory, protocol.SessionHistoryData{SessionID: "session-author"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionHistory {
		t.Fatalf("expected one session_history envelope, got %+v", envs)
	}
	var data protocol.SessionHistoryData
	if err := json.Unmarshal(envs[0].Data, &data); err != nil {
		t.Fatalf("unmarshaling session_history: %v", err)
	}
	if len(data.Messages) == 0 {
		t.Fatalf("session_history came back with no messages")
	}
	first := data.Messages[0]
	if first.Role != "user" || first.Author != "aveline" || first.AuthorAvatar != "🐣" {
		t.Fatalf("first history message = %+v, want the sender's name and avatar", first)
	}
	for _, m := range data.Messages[1:] {
		if m.Author != "" || m.AuthorAvatar != "" {
			t.Fatalf("assistant/tool message carries an author: %+v", m)
		}
	}
}

// An oversized avatar is cosmetic, so it's dropped rather than failing the
// prompt — the message is still stored, just without a picture.
func TestServeConnection_OversizedPromptAvatarIsDroppedNotRejected(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("getting default project: %v", err)
	}

	huge := "data:image/png;base64," + strings.Repeat("A", maxAvatarLen)
	if _, err := pc.SessStore.Update("session-big-avatar", func(state *agent.SessionState) error {
		state.History = []agent.Message{{Role: "user", Content: "hi"}}
		return nil
	}); err != nil {
		t.Fatalf("seeding session: %v", err)
	}

	// The size check lives on the prompt path; assert it through a real prompt
	// against a provider that fails instantly (the stored message is what
	// matters, not the model's reply).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	runDispatchWaiting(t, deps,
		envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
			Name: "test-anthropic-2", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic",
			APIKey: "sk-ant-test", BaseURL: srv.URL,
		}),
		envelopeJSONWithSession(t, protocol.EventSetProvider, "session-big-avatar", protocol.SetProviderData{
			ProviderName: "test-anthropic-2", Model: "claude-sonnet-5",
		}),
		envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
			Prompt: "hi", SessionID: "session-big-avatar",
			AuthorName: "aveline", AuthorAvatar: huge,
		}),
	)

	state, err := pc.SessStore.Load("session-big-avatar")
	if err != nil {
		t.Fatalf("loading session: %v", err)
	}
	var user *agent.Message
	for i := range state.History {
		if state.History[i].Role == "user" {
			user = &state.History[i]
		}
	}
	if user == nil {
		t.Fatalf("no user message stored: %+v", state.History)
	}
	if user.AuthorName != "aveline" {
		t.Fatalf("author name = %q, want it kept", user.AuthorName)
	}
	if user.AuthorAvatar != "" {
		t.Fatalf("oversized avatar was stored (%d bytes), want it dropped", len(user.AuthorAvatar))
	}
}
