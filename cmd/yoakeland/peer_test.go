package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/protocol"
)

// fakeIPCPeer wires a PeerRegistry entry to an in-memory buffer so a test
// can inspect exactly what got broadcast to it.
func fakeIPCPeer(t *testing.T, r *PeerRegistry, name, deviceType string) (*Peer, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	p := r.Add(ipc, &fakeCloser{}, "unix", name, deviceType, "")
	return p, out
}

func lastPeerList(t *testing.T, out *bytes.Buffer) protocol.PeerListData {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		t.Fatalf("no events broadcast")
	}
	var env protocol.Envelope
	if err := json.Unmarshal(lines[len(lines)-1], &env); err != nil {
		t.Fatalf("unmarshaling envelope: %v", err)
	}
	if env.Type != protocol.EventPeerList {
		t.Fatalf("expected peer_list, got %s", env.Type)
	}
	var data protocol.PeerListData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshaling peer_list data: %v", err)
	}
	return data
}

// TestPeerRegistry_AddBroadcastsFullRosterToEveryone guards the checklist's
// core ask: peer_list is pushed on change to every connected peer, not just
// the one that joined, and reflects the full current roster.
func TestPeerRegistry_AddBroadcastsFullRosterToEveryone(t *testing.T) {
	r := NewPeerRegistry()

	first, firstOut := fakeIPCPeer(t, r, "alice-laptop", "desktop")
	_ = first

	_, secondOut := fakeIPCPeer(t, r, "", "web")

	// The second Add should have re-broadcast to the first peer too, so its
	// buffer now ends with a roster containing both peers.
	data := lastPeerList(t, firstOut)
	if len(data.Peers) != 2 {
		t.Fatalf("expected first peer to see a 2-peer roster after the second joined, got %+v", data.Peers)
	}

	data2 := lastPeerList(t, secondOut)
	if len(data2.Peers) != 2 {
		t.Fatalf("expected second peer's own initial push to already include both peers, got %+v", data2.Peers)
	}

	var sawGeneratedName bool
	for _, p := range data2.Peers {
		if p.DeviceType == "web" && p.Name != "" {
			sawGeneratedName = true
		}
	}
	if !sawGeneratedName {
		t.Fatalf("expected an empty client-supplied name to get a generated placeholder, got %+v", data2.Peers)
	}
}

// TestPeerRegistry_AddDropsOversizedAvatar guards that a client sending an
// absurdly large avatar (see maxAvatarLen) doesn't get it stored/rebroadcast
// to every other peer — it's dropped, not rejected outright, since a bad
// avatar shouldn't fail the whole connection.
func TestPeerRegistry_AddDropsOversizedAvatar(t *testing.T) {
	r := NewPeerRegistry()
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	huge := strings.Repeat("a", maxAvatarLen+1)

	p := r.Add(ipc, &fakeCloser{}, "unix", "someone", "web", huge)
	if p.Avatar != "" {
		t.Fatalf("expected an oversized avatar to be dropped, got %d bytes stored", len(p.Avatar))
	}
}

// TestPeerRegistry_SetAvatar guards that a peer can update its own avatar
// post-connect and the change shows up in the next broadcast.
func TestPeerRegistry_SetAvatar(t *testing.T) {
	r := NewPeerRegistry()
	p, out := fakeIPCPeer(t, r, "someone", "desktop")

	if !r.SetAvatar(p.ID, "data:image/png;base64,abc123") {
		t.Fatal("expected SetAvatar of a live peer to succeed")
	}
	data := lastPeerList(t, out)
	if len(data.Peers) != 1 || data.Peers[0].Avatar != "data:image/png;base64,abc123" {
		t.Fatalf("expected the avatar reflected in the roster, got %+v", data.Peers)
	}

	if r.SetAvatar("does-not-exist", "whatever") {
		t.Fatal("expected SetAvatar of an unknown peer to report false")
	}
}

// TestPeerRegistry_Rename guards that a peer can rename itself, that the
// change is visible in the next broadcast, and that renaming an unknown ID
// is reported rather than silently ignored.
func TestPeerRegistry_Rename(t *testing.T) {
	r := NewPeerRegistry()
	p, out := fakeIPCPeer(t, r, "old-name", "desktop")

	if !r.Rename(p.ID, "new-name") {
		t.Fatal("expected Rename of a live peer to succeed")
	}
	data := lastPeerList(t, out)
	if len(data.Peers) != 1 || data.Peers[0].Name != "new-name" {
		t.Fatalf("expected the rename reflected in the roster, got %+v", data.Peers)
	}

	if r.Rename("does-not-exist", "whatever") {
		t.Fatal("expected Rename of an unknown peer to report false")
	}
}

// TestPeerRegistry_DisconnectClosesConnAndTriggersRemoval guards that
// Disconnect force-closes the target's underlying connection (the owning
// acceptLoop goroutine is responsible for then calling Remove, per its own
// deferred cleanup — simulated here directly since there's no real
// goroutine in this unit test).
func TestPeerRegistry_DisconnectClosesConnAndTriggersRemoval(t *testing.T) {
	r := NewPeerRegistry()
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	closer := &fakeCloser{}
	p := r.Add(ipc, closer, "unix", "target", "desktop", "")

	if !r.Disconnect(p.ID) {
		t.Fatal("expected Disconnect of a live peer to succeed")
	}
	if !closer.closed.Load() {
		t.Fatal("expected Disconnect to close the peer's underlying connection")
	}
	if r.Disconnect("does-not-exist") {
		t.Fatal("expected Disconnect of an unknown peer to report false")
	}

	// Mirrors what acceptLoop's defer does on the real close.
	r.Remove(p.ID)
	if len(r.All()) != 0 {
		t.Fatalf("expected the roster empty after Remove, got %+v", r.All())
	}
}
