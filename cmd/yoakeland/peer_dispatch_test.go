package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// runDispatchNetwork is runDispatch with a caller-chosen network, for
// exercising the local-socket-only gate on set_remote_access/
// set_remote_password.
func runDispatchNetwork(t *testing.T, deps connDeps, network string, lines ...string) []protocol.Envelope {
	t.Helper()
	in := bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(in, out)

	serveConnection(context.Background(), ipc, deps, network, "", nil)

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

// TestServeConnection_SetPeerNameWithoutAPeerIsAnError guards that
// set_peer_name on a connection with no registered peer (peerID == "" —
// dispatch tests here never go through acceptLoop's registration) is
// reported, not silently swallowed.
func TestServeConnection_SetPeerNameWithoutAPeerIsAnError(t *testing.T) {
	deps := newTestDeps(t)
	line := envelopeJSON(t, protocol.EventSetPeerName, protocol.SetPeerNameData{Name: "new-name"})

	envs := runDispatch(t, deps, line)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error with no registered peer, got %+v", envs)
	}
}

// TestServeConnection_SetPeerNameRenamesRegisteredPeer guards the success
// path end to end through dispatch: a connection that does have a
// registered peer (simulated by registering one directly, same as
// acceptLoop would) gets renamed and sees the update in its own broadcast.
func TestServeConnection_SetPeerNameRenamesRegisteredPeer(t *testing.T) {
	deps := newTestDeps(t)
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	peer := deps.peers.Add(ipc, &fakeCloser{}, "unix", "old-name", "desktop", "")

	in := bytes.NewBufferString(envelopeJSON(t, protocol.EventSetPeerName, protocol.SetPeerNameData{Name: "renamed"}) + "\n")
	ipc2 := protocol.NewIPCStream(in, out)
	serveConnection(context.Background(), ipc2, deps, "unix", peer.ID, nil)

	if peer.Name != "renamed" {
		t.Fatalf("expected peer.Name = %q, got %q", "renamed", peer.Name)
	}
}

// TestServeConnection_SetPeerAvatar guards the success path and the size
// guard: a registered peer can set its avatar, and an oversized one is
// rejected with an error instead of being silently truncated or accepted.
func TestServeConnection_SetPeerAvatar(t *testing.T) {
	deps := newTestDeps(t)
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	peer := deps.peers.Add(ipc, &fakeCloser{}, "unix", "someone", "desktop", "")

	line := envelopeJSON(t, protocol.EventSetPeerAvatar, protocol.SetPeerAvatarData{Avatar: "data:image/png;base64,abc123"})
	in := bytes.NewBufferString(line + "\n")
	ipc2 := protocol.NewIPCStream(in, out)
	serveConnection(context.Background(), ipc2, deps, "unix", peer.ID, nil)

	if peer.Avatar != "data:image/png;base64,abc123" {
		t.Fatalf("expected peer.Avatar set, got %q", peer.Avatar)
	}

	oversizedLine := envelopeJSON(t, protocol.EventSetPeerAvatar, protocol.SetPeerAvatarData{Avatar: strings.Repeat("a", maxAvatarLen+1)})
	envs := runDispatch(t, deps, oversizedLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error for an oversized avatar, got %+v", envs)
	}
}

// TestServeConnection_DisconnectUnknownPeerIsAnError guards that
// disconnect_peer on a peerId that isn't live is reported, not a silent
// no-op — the caller should be able to tell "already gone" from "worked."
func TestServeConnection_DisconnectUnknownPeerIsAnError(t *testing.T) {
	deps := newTestDeps(t)
	line := envelopeJSON(t, protocol.EventDisconnectPeer, protocol.DisconnectPeerData{PeerID: "never-existed"})

	envs := runDispatch(t, deps, line)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected an error for an unknown peerId, got %+v", envs)
	}
}

// TestServeConnection_DisconnectPeerClosesTarget guards the actual point:
// disconnect_peer force-closes the named peer's connection, from a
// *different* connection — any authenticated peer may disconnect any other
// (see the handler's comment on why that's the current trust model).
func TestServeConnection_DisconnectPeerClosesTarget(t *testing.T) {
	deps := newTestDeps(t)
	targetOut := &bytes.Buffer{}
	targetIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), targetOut)
	targetCloser := &fakeCloser{}
	target := deps.peers.Add(targetIPC, targetCloser, "unix", "target", "desktop", "")

	line := envelopeJSON(t, protocol.EventDisconnectPeer, protocol.DisconnectPeerData{PeerID: target.ID})
	envs := runDispatch(t, deps, line)
	if len(envs) != 0 {
		t.Fatalf("expected no error disconnecting a live peer, got %+v", envs)
	}
	if !targetCloser.closed.Load() {
		t.Fatal("expected disconnect_peer to close the target's connection")
	}
}

// TestServeConnection_SetServerIdentity guards the success path: the new
// name persists (verified via a second store handle) and is echoed back to
// every live peer, including the requester.
func TestServeConnection_SetServerIdentity(t *testing.T) {
	deps := newTestDeps(t)
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	peer := deps.peers.Add(ipc, &fakeCloser{}, "unix", "requester", "desktop", "")

	in := bytes.NewBufferString(envelopeJSON(t, protocol.EventSetServerIdentity, protocol.SetServerIdentityData{Name: "living-room-server"}) + "\n")
	ipc2 := protocol.NewIPCStream(in, out)
	serveConnection(context.Background(), ipc2, deps, "unix", peer.ID, nil)

	if got := deps.settings.GetIdentity(); got != "living-room-server" {
		t.Fatalf("GetIdentity() = %q, want %q", got, "living-room-server")
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var saw bool
	for _, l := range lines {
		var env protocol.Envelope
		_ = json.Unmarshal([]byte(l), &env)
		if env.Type == protocol.EventServerIdentity {
			var data protocol.ServerIdentityData
			_ = json.Unmarshal(env.Data, &data)
			if data.Identity == "living-room-server" {
				saw = true
			}
		}
	}
	if !saw {
		t.Fatalf("expected a server_identity push with the new name, got: %s", out.String())
	}
}

// TestServeConnection_RemoteAccessAndPasswordAreLocalSocketOnly guards the
// permission decision from the server checklist: a remote (TCP) peer must
// not be able to open/close the listener it's connected through, or rotate
// the password that gates it — only a local (unix) connection can.
func TestServeConnection_RemoteAccessAndPasswordAreLocalSocketOnly(t *testing.T) {
	deps := newTestDeps(t)

	accessLine := envelopeJSON(t, protocol.EventSetRemoteAccess, protocol.SetRemoteAccessData{Enabled: true})
	envs := runDispatchNetwork(t, deps, "tcp", accessLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected set_remote_access over tcp to be rejected, got %+v", envs)
	}
	if deps.remote.ln != nil {
		t.Fatal("expected the TCP listener to stay closed after a rejected remote set_remote_access")
	}

	passwordLine := envelopeJSON(t, protocol.EventSetRemotePassword, protocol.SetRemotePasswordData{Password: "new-password"})
	envs = runDispatchNetwork(t, deps, "tcp", passwordLine)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("expected set_remote_password over tcp to be rejected, got %+v", envs)
	}
	if deps.settings.GetToken() == "new-password" {
		t.Fatal("expected the rejected remote set_remote_password to not take effect")
	}
}

// TestServeConnection_SetRemotePasswordOverUnix guards the success path:
// a local connection can rotate the password, and it's persisted.
func TestServeConnection_SetRemotePasswordOverUnix(t *testing.T) {
	deps := newTestDeps(t)
	line := envelopeJSON(t, protocol.EventSetRemotePassword, protocol.SetRemotePasswordData{Password: "new-password"})

	envs := runDispatch(t, deps, line)
	if len(envs) != 1 || envs[0].Type != protocol.EventRemotePasswordSet {
		t.Fatalf("expected remote_password_set, got %+v", envs)
	}
	if got := deps.settings.GetToken(); got != "new-password" {
		t.Fatalf("GetToken() = %q, want %q", got, "new-password")
	}
}
