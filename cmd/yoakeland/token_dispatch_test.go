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
// per-person tokens: a registered token's name is the initial name for a
// device that reports none, a device that reports its own identity keeps it,
// and an unrecognized token is still rejected exactly as before.
func TestHandshake_ResolvesNamedTokenIdentity(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.settings.AddToken("aveline", "tok-123", "aveline@example.com"); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	if !deps.settings.SetTokenAvatar("aveline", "data:image/png;base64,abc") {
		t.Fatal("SetTokenAvatar should report the token existed")
	}

	line := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "tok-123"})
	who, _, ok := handshake(protocol.NewIPCStream(bytes.NewBufferString(line+"\n"), &bytes.Buffer{}), "tcp", deps.settings.LookupToken)
	if !ok || who.Name != "aveline" || who.TokenName != "aveline" || who.Email != "aveline@example.com" || who.Avatar == "" {
		t.Fatalf("handshake() = %+v ok=%v, want the token's name and saved avatar for a device that sent none", who, ok)
	}

	own := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "tok-123", Name: "fish", Avatar: "data:image/png;base64,own"})
	who, _, ok = handshake(protocol.NewIPCStream(bytes.NewBufferString(own+"\n"), &bytes.Buffer{}), "tcp", deps.settings.LookupToken)
	if !ok || who.Name != "fish" || who.Avatar != "data:image/png;base64,own" || who.TokenName != "aveline" {
		t.Fatalf("handshake() = %+v ok=%v, want the device's own name and avatar with the credential name kept", who, ok)
	}

	// A token with no saved avatar shows the one the device sent on connect.
	if err := deps.settings.AddToken("phone", "tok-456", ""); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	fresh := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "tok-456", Avatar: "data:image/png;base64,mine"})
	who, _, ok = handshake(protocol.NewIPCStream(bytes.NewBufferString(fresh+"\n"), &bytes.Buffer{}), "tcp", deps.settings.LookupToken)
	if !ok || who.Avatar != "data:image/png;base64,mine" {
		t.Fatalf("avatar = %q ok=%v, want the self-reported avatar for a token with none saved", who.Avatar, ok)
	}

	badLine := envelopeJSON(t, protocol.EventAuth, protocol.AuthData{Token: "wrong"})
	badIPC := protocol.NewIPCStream(bytes.NewBufferString(badLine+"\n"), &bytes.Buffer{})
	_, _, ok = handshake(badIPC, "tcp", deps.settings.LookupToken)
	if ok {
		t.Fatal("expected an unrecognized token to fail the handshake")
	}
}
