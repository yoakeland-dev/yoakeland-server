package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

func TestRemoteScope(t *testing.T) {
	deps := newTestDeps(t)
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", DeviceType: "phone", Restricted: true})
	person := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "laptop", DeviceType: "desktop"})

	for _, tc := range []struct {
		name    string
		network string
		peer    string
		event   protocol.EventType
		denied  bool
	}{
		{"phone may chat", "tcp", phone.ID, protocol.EventUserPrompt, false},
		{"phone cannot add an MCP server", "tcp", phone.ID, protocol.EventSaveMCPServer, true},
		{"phone cannot register a provider", "tcp", phone.ID, protocol.EventRegisterProvider, true},
		{"phone cannot change runtime settings", "tcp", phone.ID, protocol.EventSetRuntimeSettings, true},
		{"phone cannot add a global rule", "tcp", phone.ID, protocol.EventAddPermissionRule, true},
		{"phone cannot open a terminal", "tcp", phone.ID, protocol.EventTerminalOpen, true},
		{"phone cannot type into one", "tcp", phone.ID, protocol.EventTerminalInput, true},
		{"phone cannot drop a peer", "tcp", phone.ID, protocol.EventDisconnectPeer, true},
		{"an unlisted event is denied", "tcp", phone.ID, protocol.EventType("something_new"), true},
		{"an unregistered tcp connection is denied", "tcp", "", protocol.EventSaveMCPServer, true},
		{"a full credential is not restricted", "tcp", person.ID, protocol.EventSaveMCPServer, false},
		{"the local socket is not restricted", "unix", "", protocol.EventSaveMCPServer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteDenied(deps, tc.network, tc.peer, tc.event); got != tc.denied {
				t.Fatalf("remoteDenied = %v, want %v", got, tc.denied)
			}
		})
	}
}

// The gate must reach the handler path, not just the helper.
func TestRemoteScope_DispatchRefuses(t *testing.T) {
	deps := newTestDeps(t)
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", Restricted: true})
	out := &bytes.Buffer{}
	in := bytes.NewBufferString(`{"type":"save_mcp_server","data":{}}` + "\n")
	serveConnection(context.Background(), protocol.NewIPCStream(in, out), deps, "tcp", phone.ID, nil)
	var env protocol.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil || env.Type != protocol.EventError || !strings.Contains(string(env.Data), "isn't available from a paired device") {
		t.Fatalf("expected a refusal, got %q (%v)", out.String(), err)
	}
}

func TestHandshake_PairingCredentialIsRestricted(t *testing.T) {
	for _, tc := range []struct {
		pairing bool
	}{{true}, {false}} {
		lookup := func(string) (string, TokenRecord, bool) { return "tok", TokenRecord{Pairing: tc.pairing}, true }
		in := bytes.NewBufferString(`{"type":"auth","data":{"token":"x"}}` + "\n")
		who, _, ok := handshake(protocol.NewIPCStream(in, &bytes.Buffer{}), "tcp", lookup)
		if !ok || who.Restricted != tc.pairing {
			t.Fatalf("pairing=%v: ok=%v restricted=%v", tc.pairing, ok, who.Restricted)
		}
	}
}
