package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// newTestDeps builds a connDeps with one pre-opened default project (a
// fresh temp dir) and its own server-level provider/oauth stores, mirroring
// what main() assembles for real, minus any actual network listener.
func newTestDeps(t *testing.T) connDeps {
	t.Helper()
	tempDir := t.TempDir()
	stateDir := t.TempDir()

	projects, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatalf("project registry: %v", err)
	}
	if _, err := projects.Get(tempDir); err != nil {
		t.Fatalf("opening default test project: %v", err)
	}

	providerStore, err := provider.NewProviderStore(stateDir, "")
	if err != nil {
		t.Fatalf("provider store: %v", err)
	}

	settings, err := NewServerSettings(stateDir)
	if err != nil {
		t.Fatalf("server settings: %v", err)
	}

	peers := NewPeerRegistry()
	deps := connDeps{
		projects:           projects,
		defaultProjectRoot: tempDir,
		providerStore:      providerStore,
		oauthStore:         provider.NewOAuthSessionStore(),
		manifestCache:      provider.NewManifestCache(""),
		peers:              peers,
		settings:           settings,
		turnWG:             &sync.WaitGroup{},
	}
	deps.remote = &remoteControl{settings: settings, deps: deps}
	return deps
}

// runDispatchDiscard is for envelopes (like a successful user_prompt) whose
// handling spawns a goroutine that keeps writing after serveConnection
// returns — reading a bytes.Buffer afterward would race with that
// goroutine's write. io.Discard has no shared state to race on, so this is
// for priming state (e.g. binding a session to a project) without caring
// about the response. It waits for deps.turnWG (set by newTestDeps) before
// returning, so a test's own filesystem/registry assertions right after
// this call aren't racing that goroutine either.
func runDispatchDiscard(t *testing.T, deps connDeps, lines ...string) {
	t.Helper()
	in := bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	ipc := protocol.NewIPCStream(in, io.Discard)
	serveConnection(context.Background(), ipc, deps, "unix", "", nil)
	if deps.turnWG != nil {
		deps.turnWG.Wait()
	}
}

// runDispatch feeds newline-delimited envelopes through serveConnection and
// returns every envelope it wrote back out. Only use this when nothing in
// `lines` triggers a goroutine that outlives serveConnection's return (e.g.
// a successful user_prompt) — otherwise use runDispatchDiscard instead.
func runDispatch(t *testing.T, deps connDeps, lines ...string) []protocol.Envelope {
	t.Helper()
	in := bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(in, out)

	serveConnection(context.Background(), ipc, deps, "unix", "", nil)

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

func envelopeJSON(t *testing.T, envType protocol.EventType, data any) string {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshaling data: %v", err)
	}
	env := protocol.Envelope{Type: envType, Data: raw}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}
	return string(out)
}

func TestServeConnection_RegisterListSetProvider(t *testing.T) {
	deps := newTestDeps(t)

	registerLine := envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{
		Name:         "my-anthropic",
		Kind:         "api_key",
		HTTPProvider: "anthropic",
		APIKey:       "sk-ant-test-key",
	})
	listLine := envelopeJSON(t, protocol.EventListProviders, struct{}{})
	setLine := envelopeJSON(t, protocol.EventSetProvider, protocol.SetProviderData{ProviderName: "my-anthropic", Model: "claude-sonnet-5"})
	setUnknownLine := envelopeJSON(t, protocol.EventSetProvider, protocol.SetProviderData{ProviderName: "does-not-exist"})

	envs := runDispatch(t, deps, registerLine, listLine, setLine, setUnknownLine)

	if len(envs) != 4 {
		t.Fatalf("expected 4 response envelopes, got %d: %+v", len(envs), envs)
	}

	if envs[0].Type != protocol.EventProviderRegistered {
		t.Fatalf("expected provider_registered, got %s", envs[0].Type)
	}
	var registeredData protocol.ProviderRegisteredData
	_ = json.Unmarshal(envs[0].Data, &registeredData)
	if registeredData.Name != "my-anthropic" {
		t.Fatalf("unexpected registered name: %+v", registeredData)
	}

	if envs[1].Type != protocol.EventProviderList {
		t.Fatalf("expected provider_list, got %s", envs[1].Type)
	}
	var listData protocol.ProviderListData
	_ = json.Unmarshal(envs[1].Data, &listData)
	if len(listData.Providers) != 1 || listData.Providers[0].Name != "my-anthropic" {
		t.Fatalf("unexpected provider list: %+v", listData)
	}

	if envs[2].Type != protocol.EventProviderSet {
		t.Fatalf("expected provider_set, got %s (data: %s)", envs[2].Type, envs[2].Data)
	}
	var setData protocol.ProviderSetData
	_ = json.Unmarshal(envs[2].Data, &setData)
	if setData.ProviderName != "my-anthropic" {
		t.Fatalf("unexpected provider_set data: %+v", setData)
	}

	if envs[3].Type != protocol.EventError {
		t.Fatalf("expected an error for an unknown provider name, got %s", envs[3].Type)
	}
}

func TestServeConnection_OAuthStartAndUnknownComplete(t *testing.T) {
	deps := newTestDeps(t)

	startLine := envelopeJSON(t, protocol.EventOAuthStart, protocol.OAuthStartData{
		Name:        "my-openrouter",
		CallbackURL: "https://app.example.com/callback",
	})
	completeUnknownLine := envelopeJSON(t, protocol.EventOAuthComplete, protocol.OAuthCompleteData{
		RequestID: "never-issued",
		Code:      "whatever",
	})

	envs := runDispatch(t, deps, startLine, completeUnknownLine)
	if len(envs) != 2 {
		t.Fatalf("expected 2 response envelopes, got %d: %+v", len(envs), envs)
	}

	if envs[0].Type != protocol.EventOAuthURL {
		t.Fatalf("expected openrouter_oauth_url, got %s", envs[0].Type)
	}
	var urlData protocol.OAuthURLData
	_ = json.Unmarshal(envs[0].Data, &urlData)
	if urlData.RequestID == "" || !strings.Contains(urlData.URL, "code_challenge=") {
		t.Fatalf("unexpected oauth url data: %+v", urlData)
	}

	if envs[1].Type != protocol.EventError {
		t.Fatalf("expected an error completing an unknown oauth request, got %s", envs[1].Type)
	}
}
