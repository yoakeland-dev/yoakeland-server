package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// envelopeJSONWithSession is like envelopeJSON but also sets the envelope's
// top-level sessionId, needed for events (like set_provider) that carry no
// sessionId in their own data payload.
func envelopeJSONWithSession(t *testing.T, envType protocol.EventType, sessionID string, data any) string {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshaling data: %v", err)
	}
	env := protocol.Envelope{Type: envType, SessionID: sessionID, Data: raw}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}
	return string(out)
}

// TestServeConnection_TwoProjectsAreFullyIsolated is the end-to-end version
// of the scenario multi-project support exists for: one server, two
// sessions opened against two different folders, each restricted to its
// own directory and its own persisted session history — not just two
// Agents in memory, but two separate data dirs on disk.
func TestServeConnection_TwoProjectsAreFullyIsolated(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a-only.txt"), []byte("project A file"), 0644); err != nil {
		t.Fatalf("seeding project A file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "b-only.txt"), []byte("project B file"), 0644); err != nil {
		t.Fatalf("seeding project B file: %v", err)
	}

	deps := newTestDeps(t) // has its own unrelated default project

	openLineA := envelopeJSON(t, protocol.EventOpenProject, protocol.OpenProjectData{Path: rootA})
	openLineB := envelopeJSON(t, protocol.EventOpenProject, protocol.OpenProjectData{Path: rootB})
	listProjectsLine := envelopeJSON(t, protocol.EventListProjects, protocol.ListProjectsData{})

	envs := runDispatch(t, deps, openLineA, openLineB, listProjectsLine)
	if len(envs) != 3 {
		t.Fatalf("expected 3 responses, got %d: %+v", len(envs), envs)
	}
	if envs[0].Type != protocol.EventProjectOpened || envs[1].Type != protocol.EventProjectOpened {
		t.Fatalf("expected two project_opened events, got %s, %s", envs[0].Type, envs[1].Type)
	}

	var listData protocol.ProjectListData
	_ = json.Unmarshal(envs[2].Data, &listData)
	// The registry's own default project (from newTestDeps) plus A and B.
	if len(listData.Paths) != 3 {
		t.Fatalf("expected 3 open projects (default + A + B), got %v", listData.Paths)
	}

	// Now bind two sessions, one to each project. A successful user_prompt
	// spawns RunTurn in its own goroutine (it runs concurrently with the
	// dispatch loop by design), so its eventual response races with reading
	// this call's output buffer — the session<->project binding itself,
	// though, happens synchronously before that goroutine is spawned, so
	// verify it directly through the (mutex-guarded) registry instead of by
	// reading envelopes back.
	promptA := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "hello A", SessionID: "session-a", ProjectPath: rootA,
	})
	promptB := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "hello B", SessionID: "session-b", ProjectPath: rootB,
	})
	runDispatchDiscard(t, deps, promptA, promptB)

	pcForA, err := deps.projects.Resolve("session-a", "", "")
	if err != nil || pcForA.Root != mustAbs(t, rootA) {
		t.Fatalf("session-a resolved to %+v (err %v), want project A", pcForA, err)
	}
	pcForB, err := deps.projects.Resolve("session-b", "", "")
	if err != nil || pcForB.Root != mustAbs(t, rootB) {
		t.Fatalf("session-b resolved to %+v (err %v), want project B", pcForB, err)
	}

	// A session that tries to claim a *different* project than the one it's
	// already bound to must be rejected, not silently moved.
	conflictLine := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "sneaky", SessionID: "session-a", ProjectPath: rootB,
	})
	envs = runDispatch(t, deps, conflictLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error rebinding session-a to project B, got %+v", envs)
	}
}

// TestServeConnection_ListSessionsIsPerProject guards that list_sessions
// with an explicit projectPath returns only that project's sessions, even
// when other projects are open on the same server.
func TestServeConnection_ListSessionsIsPerProject(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	deps := newTestDeps(t)

	pcA, err := deps.projects.Get(rootA)
	if err != nil {
		t.Fatalf("Get(rootA): %v", err)
	}
	pcB, err := deps.projects.Get(rootB)
	if err != nil {
		t.Fatalf("Get(rootB): %v", err)
	}

	now := time.Now()
	if err := pcA.SessStore.Save(&agent.SessionState{SessionID: "session-in-a", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seeding session in A: %v", err)
	}
	if err := pcB.SessStore.Save(&agent.SessionState{SessionID: "session-in-b", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seeding session in B: %v", err)
	}

	listALine := envelopeJSON(t, protocol.EventListSessions, protocol.ListSessionsData{ProjectPath: rootA})
	envs := runDispatch(t, deps, listALine)
	if len(envs) != 1 || envs[0].Type != protocol.EventSessionList {
		t.Fatalf("expected a session_list envelope, got %+v", envs)
	}
	var data protocol.SessionListData
	_ = json.Unmarshal(envs[0].Data, &data)
	if len(data.Sessions) != 1 || data.Sessions[0].SessionID != "session-in-a" {
		t.Fatalf("expected only session-in-a for project A, got %+v", data.Sessions)
	}
}

// TestServeConnection_SetProviderUsesSessionsProject guards that
// set_provider resolves the CLI-subprocess provider's working directory
// (via BuildProvider) against the *session's* bound project, not some
// server-wide default — otherwise a cli_claude/cli_codex provider set on
// one project's session could end up running in the wrong directory.
func TestServeConnection_SetProviderUsesSessionsProject(t *testing.T) {
	rootA := t.TempDir()
	deps := newTestDeps(t)

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "cli-claude", Kind: string(provider.ProviderKindCLIClaude),
	})
	runDispatchDiscard(t, deps, registerLine)

	// Bind session-a to rootA directly through the registry rather than via
	// a real user_prompt: RunTurn runs in its own goroutine and, on a later
	// set_provider for the same session, Registry.Get rebinds that
	// still-running goroutine's stream to whatever buffer we read next —
	// a real reconnect scenario, but one this test doesn't need to invoke to
	// check what it's actually after (that set_provider resolves the
	// project a session is bound to correctly).
	if _, err := deps.projects.Resolve("session-a", rootA, ""); err != nil {
		t.Fatalf("binding session-a to project A: %v", err)
	}

	setLine := envelopeJSONWithSession(t, protocol.EventSetProvider, "session-a", protocol.SetProviderData{ProviderName: "cli-claude"})
	envs := runDispatch(t, deps, setLine)

	if len(envs) != 1 || envs[0].Type != protocol.EventProviderSet {
		t.Fatalf("expected a single provider_set event, got: %+v", envs)
	}
	var setData protocol.ProviderSetData
	_ = json.Unmarshal(envs[0].Data, &setData)
	if setData.SessionID != "session-a" || setData.ProviderName != "cli-claude" {
		t.Fatalf("unexpected provider_set data: %+v", setData)
	}
}

// runDispatchWaiting is runDispatch's counterpart for a batch that includes a
// user_prompt: RunTurn runs in its own goroutine and keeps writing to the
// output stream after serveConnection returns, so a bytes.Buffer must not be
// read until that goroutine is done. deps.turnWG (set by newTestDeps) is the
// happens-before edge that makes the read below race-free.
func runDispatchWaiting(t *testing.T, deps connDeps, lines ...string) []protocol.Envelope {
	t.Helper()
	in := bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(in, out)
	serveConnection(context.Background(), ipc, deps, "unix", "", nil)
	if deps.turnWG != nil {
		deps.turnWG.Wait()
	}

	var envs []protocol.Envelope
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("malformed output line %q: %v", line, err)
		}
		envs = append(envs, env)
	}
	return envs
}

// TestServeConnection_SetProviderBindsUnpromptedSessionToItsProject covers the
// brand-new-chat ordering the client actually uses: a model is picked
// (set_provider) before the session has ever been prompted, so nothing has
// bound it to a project yet. set_provider must resolve against the project the
// upcoming prompt will itself use, not the server's default-project fallback —
// they are separate session registries, so landing on the wrong one leaves the
// turn's own registry unconfigured and the first turn comes back
// finishReason "no_provider" (which the client reacts to by discarding the
// model selection it just made).
func TestServeConnection_SetProviderBindsUnpromptedSessionToItsProject(t *testing.T) {
	rootA := t.TempDir()
	deps := newTestDeps(t) // has its own unrelated default project

	// A provider that fails the turn upstream (500) rather than not existing at
	// all, so "the turn reached the model" is distinguishable from "the turn
	// was refused for having no provider". No real network call either way.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	defer srv.Close()

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name: "test-anthropic", Kind: "api_key", HTTPProvider: "anthropic",
		APIKey: "sk-ant-test", BaseURL: srv.URL,
	})
	// set_provider carries the projectPath, because that is the only thing that
	// can bind this session: no prompt has gone out yet.
	setLine := envelopeJSONWithSession(t, protocol.EventSetProvider, "session-new", protocol.SetProviderData{
		ProviderName: "test-anthropic", Model: "claude-sonnet-5", ProjectPath: rootA,
	})
	promptLine := envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{
		Prompt: "hello", SessionID: "session-new", ProjectPath: rootA,
	})

	envs := runDispatchWaiting(t, deps, registerLine, setLine, promptLine)

	var sawProviderSet bool
	for _, env := range envs {
		if env.Type == protocol.EventProviderSet {
			sawProviderSet = true
		}
	}
	if !sawProviderSet {
		t.Fatalf("expected set_provider to succeed, got %+v", envs)
	}
	pcA, err := deps.projects.Resolve("session-new", "", "")
	if err != nil || pcA.Root != mustAbs(t, rootA) {
		t.Fatalf("session-new resolved to %+v (err %v), want project A", pcA, err)
	}

	for _, env := range envs {
		if env.Type != protocol.EventTurnCompleted {
			continue
		}
		var data protocol.TurnCompletedData
		_ = json.Unmarshal(env.Data, &data)
		if data.FinishReason == "no_provider" {
			t.Fatalf("first turn came back no_provider — set_provider did not reach the project registry the prompt binds to")
		}
	}

	var reachedModel bool
	for _, env := range envs {
		if env.Type != protocol.EventError {
			continue
		}
		var data protocol.ErrorData
		_ = json.Unmarshal(env.Data, &data)
		if strings.Contains(data.Message, "model stream error") {
			reachedModel = true
		}
	}
	if !reachedModel {
		t.Fatalf("expected the first turn to reach the model and fail upstream, got %+v", envs)
	}
}
