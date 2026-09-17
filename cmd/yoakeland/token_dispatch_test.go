package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"yoakeland/internal/protocol"
)

// TestServeConnection_RegisterListRemoveToken guards the whole
// register_token -> list_tokens -> remove_token lifecycle over the local
// socket: a fresh token is generated and returned once, shows up scrubbed
// (no raw token) in list_tokens, and removal is idempotent (mirroring
// remove_provider) — matching the local-socket-only, plaintext-behind-file-
// perms posture set_remote_password already uses.
func TestServeConnection_RegisterListRemoveToken(t *testing.T) {
	deps := newTestDeps(t)

	registerLine := envelopeJSON(t, protocol.EventRegisterToken, protocol.RegisterTokenData{Name: "aveline", Email: "aveline@example.com"})
	envs := runDispatch(t, deps, registerLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventTokenRegistered {
		t.Fatalf("register_token: expected token_registered, got %+v", envs)
	}
	var registered protocol.TokenRegisteredData
	if err := json.Unmarshal(envs[0].Data, &registered); err != nil {
		t.Fatalf("unmarshal token_registered: %v", err)
	}
	if registered.Name != "aveline" || registered.Token == "" {
		t.Fatalf("token_registered = %+v, want a non-empty token for aveline", registered)
	}

	listLine := envelopeJSON(t, protocol.EventListTokens, struct{}{})
	envs = runDispatch(t, deps, listLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventTokenList {
		t.Fatalf("list_tokens: expected token_list, got %+v", envs)
	}
	var listed protocol.TokenListData
	if err := json.Unmarshal(envs[0].Data, &listed); err != nil {
		t.Fatalf("unmarshal token_list: %v", err)
	}
	if len(listed.Tokens) != 1 || listed.Tokens[0].Name != "aveline" {
		t.Fatalf("token_list = %+v, want one entry for aveline", listed)
	}

	removeLine := envelopeJSON(t, protocol.EventRemoveToken, protocol.RemoveTokenData{Name: "aveline"})
	envs = runDispatch(t, deps, removeLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventTokenRemoved {
		t.Fatalf("remove_token: expected token_removed, got %+v", envs)
	}

	// Idempotent: removing an already-gone name still succeeds.
	envs = runDispatch(t, deps, removeLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventTokenRemoved {
		t.Fatalf("remove_token (second call): expected token_removed, got %+v", envs)
	}
}

// TestServeConnection_TokenCommandsAreLocalSocketOnly guards that a remote
// peer can't mint, list, or revoke credentials for itself — same
// restriction as set_remote_access/set_remote_password.
func TestServeConnection_TokenCommandsAreLocalSocketOnly(t *testing.T) {
	deps := newTestDeps(t)

	cases := []struct {
		name string
		line string
	}{
		{"register_token", envelopeJSON(t, protocol.EventRegisterToken, protocol.RegisterTokenData{Name: "aveline"})},
		{"list_tokens", envelopeJSON(t, protocol.EventListTokens, struct{}{})},
		{"remove_token", envelopeJSON(t, protocol.EventRemoveToken, protocol.RemoveTokenData{Name: "aveline"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envs := runDispatchNetwork(t, deps, "tcp", tc.line)
			if len(envs) != 1 || envs[0].Type != protocol.EventError {
				t.Fatalf("%s over tcp: expected an error, got %+v", tc.name, envs)
			}
		})
	}
}

// TestHandshake_ResolvesNamedTokenIdentity guards the actual point of
// per-person tokens: a registered token's persisted name/avatar wins over
// whatever the client self-reports in the auth frame, and an unrecognized
// token is still rejected exactly as before.
func TestHandshake_ResolvesNamedTokenIdentity(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.settings.AddToken("aveline", "tok-123", "aveline@example.com"); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	if !deps.settings.SetTokenAvatar("aveline", "data:image/png;base64,abc") {
		t.Fatal("SetTokenAvatar should report the token existed")
	}

	line := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "tok-123", Name: "spoofed-name"})
	ipc := protocol.NewIPCStream(bytes.NewBufferString(line+"\n"), &bytes.Buffer{})
	name, _, avatar, tokenName, email, _, ok := handshake(ipc, "tcp", deps.settings.LookupToken)
	if !ok || name != "aveline" || tokenName != "aveline" || email != "aveline@example.com" || avatar == "" {
		t.Fatalf("handshake() = name=%q tokenName=%q email=%q avatar=%q ok=%v, want the persisted identity to win over the self-reported name", name, tokenName, email, avatar, ok)
	}

	badLine := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "wrong"})
	badIPC := protocol.NewIPCStream(bytes.NewBufferString(badLine+"\n"), &bytes.Buffer{})
	_, _, _, _, _, _, ok = handshake(badIPC, "tcp", deps.settings.LookupToken)
	if ok {
		t.Fatal("expected an unrecognized token to fail the handshake")
	}
}
