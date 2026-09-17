package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

func TestServeConnection_ListSessions(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("getting default project: %v", err)
	}

	if err := pc.SessStore.Save(&agent.SessionState{
		SessionID: "session-alpha",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		History: []agent.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}); err != nil {
		t.Fatalf("seeding a session: %v", err)
	}

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListSessions, struct{}{}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionList {
		t.Fatalf("expected a single session_list envelope, got %+v", envs)
	}

	var data protocol.SessionListData
	if err := json.Unmarshal(envs[0].Data, &data); err != nil {
		t.Fatalf("unmarshaling session_list data: %v", err)
	}
	if len(data.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d: %+v", len(data.Sessions), data.Sessions)
	}
	if data.Sessions[0].SessionID != "session-alpha" || data.Sessions[0].MessageCount != 2 {
		t.Fatalf("unexpected session summary: %+v", data.Sessions[0])
	}
}

func TestServeConnection_GetSessionHistory(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("getting default project: %v", err)
	}

	if err := pc.SessStore.Save(&agent.SessionState{
		SessionID: "session-beta",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		History: []agent.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "file_read", Args: json.RawMessage(`{"path":"a.go"}`)}}},
		},
	}); err != nil {
		t.Fatalf("seeding a session: %v", err)
	}

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "session-beta"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionHistory {
		t.Fatalf("expected a single session_history envelope, got %+v", envs)
	}

	var data protocol.SessionHistoryData
	if err := json.Unmarshal(envs[0].Data, &data); err != nil {
		t.Fatalf("unmarshaling session_history data: %v", err)
	}
	if data.SessionID != "session-beta" || len(data.Messages) != 2 {
		t.Fatalf("unexpected session_history data: %+v", data)
	}
	if data.Messages[1].ToolCalls[0].Name != "file_read" {
		t.Fatalf("expected tool call round-tripped in history, got %+v", data.Messages[1])
	}
}

// TestServeConnection_GetSessionHistory_UnknownSession guards that asking
// for a session with no persisted history yet returns an empty list, not an
// error — a client should be able to call this unconditionally on open.
func TestServeConnection_GetSessionHistory_UnknownSession(t *testing.T) {
	deps := newTestDeps(t)

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "never-seen"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionHistory {
		t.Fatalf("expected a single session_history envelope, got %+v", envs)
	}
	var data protocol.SessionHistoryData
	_ = json.Unmarshal(envs[0].Data, &data)
	if len(data.Messages) != 0 {
		t.Fatalf("expected no messages for an unknown session, got %+v", data.Messages)
	}
}

func TestServeConnection_SetSessionLayout(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("getting default project: %v", err)
	}

	for _, id := range []string{"session-main", "session-scratch"} {
		if err := pc.SessStore.Save(&agent.SessionState{SessionID: id, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}

	linked := "session-scratch"
	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventSetSessionLayout, protocol.SetSessionLayoutData{
		SessionID:       "session-main",
		LinkedSessionID: &linked,
	}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionUpdated {
		t.Fatalf("expected a single session_updated envelope, got %+v", envs)
	}
	var data protocol.SessionSummary
	if err := json.Unmarshal(envs[0].Data, &data); err != nil {
		t.Fatalf("unmarshaling session_updated data: %v", err)
	}
	if data.LinkedSessionID == nil || *data.LinkedSessionID != "session-scratch" {
		t.Fatalf("expected linkedSessionId to round-trip, got %+v", data)
	}

	// Persisted, not just echoed back.
	reloaded, err := pc.SessStore.Load("session-main")
	if err != nil {
		t.Fatalf("reloading session-main: %v", err)
	}
	if reloaded.LinkedSessionID == nil || *reloaded.LinkedSessionID != "session-scratch" {
		t.Fatalf("expected linkedSessionId persisted, got %+v", reloaded.LinkedSessionID)
	}

	// Unpair with an explicit null.
	envs = runDispatch(t, deps, envelopeJSON(t, protocol.EventSetSessionLayout, protocol.SetSessionLayoutData{
		SessionID:       "session-main",
		LinkedSessionID: nil,
	}))
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionUpdated {
		t.Fatalf("expected a single session_updated envelope, got %+v", envs)
	}
	data = protocol.SessionSummary{}
	_ = json.Unmarshal(envs[0].Data, &data)
	if data.LinkedSessionID != nil {
		t.Fatalf("expected linkedSessionId cleared, got %+v", data.LinkedSessionID)
	}
}

func TestServeConnection_DeleteSession(t *testing.T) {
	deps := newTestDeps(t)
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("getting default project: %v", err)
	}

	for _, s := range []*agent.SessionState{
		{SessionID: "session-active", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{SessionID: "session-archived", CreatedAt: time.Now(), UpdatedAt: time.Now(), Archived: true},
	} {
		if err := pc.SessStore.Save(s); err != nil {
			t.Fatalf("seeding %s: %v", s.SessionID, err)
		}
	}

	for _, id := range []string{"session-active", "session-archived"} {
		envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventDeleteSession, protocol.DeleteSessionData{SessionID: id}))
		if len(envs) != 1 || envs[0].Type != protocol.EventSessionDeleted {
			t.Fatalf("deleting %s: expected a single session_deleted envelope, got %+v", id, envs)
		}
		var data protocol.SessionDeletedData
		if err := json.Unmarshal(envs[0].Data, &data); err != nil || data.SessionID != id {
			t.Fatalf("unexpected session_deleted data for %s: %+v (err=%v)", id, data, err)
		}
		if _, err := pc.SessStore.Load(id); err == nil {
			t.Fatalf("expected %s to be gone from the store after delete", id)
		}
	}

	// Deleting again (already gone) is an error, not a silent no-op.
	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventDeleteSession, protocol.DeleteSessionData{SessionID: "session-active"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error deleting an already-deleted session, got %+v", envs)
	}
}

func TestServeConnection_DetectCLI(t *testing.T) {
	deps := newTestDeps(t)

	tempDir := t.TempDir()
	for _, bin := range []string{"claude"} {
		if err := os.WriteFile(filepath.Join(tempDir, bin), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatalf("writing fake %s: %v", bin, err)
		}
	}
	t.Setenv("PATH", tempDir)

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventDetectCLI, struct{}{}))
	if len(envs) != 1 || envs[0].Type != protocol.EventCLIAvailability {
		t.Fatalf("expected a single cli_availability envelope, got %+v", envs)
	}

	var data protocol.CLIAvailabilityData
	if err := json.Unmarshal(envs[0].Data, &data); err != nil {
		t.Fatalf("unmarshaling cli_availability data: %v", err)
	}
	if !data.Claude {
		t.Fatal("expected claude to be detected on the fake PATH")
	}
	if data.Codex {
		t.Fatal("expected codex to be unavailable (not on the fake PATH)")
	}
}

func TestServeConnection_RemoveProvider(t *testing.T) {
	deps := newTestDeps(t)

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "temp-provider", Kind: "api_key", HTTPProvider: "anthropic", APIKey: "sk-ant-x",
	})
	removeLine := envelopeJSON(t, protocol.EventRemoveProvider, protocol.RemoveProviderData{Name: "temp-provider"})
	listLine := envelopeJSON(t, protocol.EventListProviders, struct{}{})

	envs := runDispatch(t, deps, registerLine, removeLine, listLine)
	if len(envs) != 3 {
		t.Fatalf("expected 3 responses, got %d: %+v", len(envs), envs)
	}
	if envs[1].Type != protocol.EventProviderRemoved {
		t.Fatalf("expected provider_removed, got %s", envs[1].Type)
	}
	var removedData protocol.ProviderRemovedData
	_ = json.Unmarshal(envs[1].Data, &removedData)
	if removedData.Name != "temp-provider" {
		t.Fatalf("unexpected provider_removed data: %+v", removedData)
	}

	var listData protocol.ProviderListData
	_ = json.Unmarshal(envs[2].Data, &listData)
	if len(listData.Providers) != 0 {
		t.Fatalf("expected the provider to be gone after removal, got %+v", listData.Providers)
	}
}

func TestServeConnection_Ping(t *testing.T) {
	deps := newTestDeps(t)

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventPing, protocol.PingData{Nonce: "abc123"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventPong {
		t.Fatalf("expected a single pong envelope, got %+v", envs)
	}
	var data protocol.PongData
	_ = json.Unmarshal(envs[0].Data, &data)
	if data.Nonce != "abc123" {
		t.Fatalf("expected pong to echo the ping's nonce, got %+v", data)
	}
}
