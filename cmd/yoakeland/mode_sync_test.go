package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/protocol"
)

// TestServeConnection_SetModeBroadcastsToOtherPeers guards the same
// multi-peer sync fix as TestServeConnection_SessionUpdateBroadcastsToOtherPeers,
// applied to set_mode: another peer with this session live must see the
// mode change without having sent the request itself.
func TestServeConnection_SetModeBroadcastsToOtherPeers(t *testing.T) {
	deps := newTestDeps(t)

	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("opening test project: %v", err)
	}
	if err := pc.SessStore.Ensure("sess-mode"); err != nil {
		t.Fatalf("creating session: %v", err)
	}

	aOut := &bytes.Buffer{}
	aIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), aOut)
	deps.peers.Add(aIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-a", DeviceType: "desktop", Avatar: ""})

	bOut := &bytes.Buffer{}
	bIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), bOut)
	peerB := deps.peers.Add(bIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-b", DeviceType: "desktop", Avatar: ""})

	line := envelopeJSON(t, protocol.EventSetMode, protocol.SetModeData{
		SessionID: "sess-mode", Mode: "autonomous",
	})
	in := bytes.NewBufferString(line + "\n")
	reqOut := &bytes.Buffer{}
	reqIPC := protocol.NewIPCStream(in, reqOut)
	serveConnection(context.Background(), reqIPC, deps, "unix", peerB.ID, nil)

	if !strings.Contains(aOut.String(), `"type":"mode_set"`) {
		t.Fatalf("expected the bystander peer to receive a broadcast mode_set, got: %s", aOut.String())
	}
	var env protocol.Envelope
	for _, l := range strings.Split(strings.TrimSpace(aOut.String()), "\n") {
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("malformed broadcast output %q: %v", l, err)
		}
		if env.Type == protocol.EventModeSet {
			break
		}
	}
	var got protocol.ModeSetData
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("malformed mode_set data: %v", err)
	}
	if got.Mode != "autonomous" {
		t.Fatalf("expected the broadcast to carry the new mode, got %+v", got)
	}
}

// TestServeConnection_ModePersistsAcrossSessionHistoryFetch guards the
// "server is the source of truth" fix: mode is written to disk immediately
// by SetMode, so a later get_session_history — as a freshly-connecting
// peer would send — reads the true current value instead of the default,
// even without any client-side cache of what was set.
func TestServeConnection_ModePersistsAcrossSessionHistoryFetch(t *testing.T) {
	deps := newTestDeps(t)

	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("opening test project: %v", err)
	}
	if err := pc.SessStore.Ensure("sess-mode-2"); err != nil {
		t.Fatalf("creating session: %v", err)
	}

	setLine := envelopeJSON(t, protocol.EventSetMode, protocol.SetModeData{
		SessionID: "sess-mode-2", Mode: "plan",
	})
	setIPC := protocol.NewIPCStream(bytes.NewBufferString(setLine+"\n"), &bytes.Buffer{})
	peer := deps.peers.Add(setIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-setter", DeviceType: "desktop", Avatar: ""})
	serveConnection(context.Background(), setIPC, deps, "unix", peer.ID, nil)

	// A brand-new connection, never involved in the SetMode call above —
	// standing in for a second device opening this session for the first
	// time.
	histLine := envelopeJSON(t, protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{
		SessionID: "sess-mode-2",
	})
	histOut := &bytes.Buffer{}
	histIPC := protocol.NewIPCStream(bytes.NewBufferString(histLine+"\n"), histOut)
	peer2 := deps.peers.Add(histIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-joiner", DeviceType: "desktop", Avatar: ""})
	serveConnection(context.Background(), histIPC, deps, "unix", peer2.ID, nil)

	var env protocol.Envelope
	for _, l := range strings.Split(strings.TrimSpace(histOut.String()), "\n") {
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("malformed output %q: %v", l, err)
		}
		if env.Type == protocol.EventSessionHistory {
			break
		}
	}
	var got protocol.SessionHistoryData
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("malformed session_history data: %v", err)
	}
	if got.Mode != "plan" {
		t.Fatalf("expected get_session_history to report the persisted mode \"plan\", got %+v", got)
	}
}

// TestServeConnection_RestoreSnapshotBroadcastsToOtherPeers guards a real
// file-reverting mutation: restore_snapshot changes on-disk content every
// peer's view of the project depends on, not just the requester's, so a
// bystander peer with this session open must be told too.
func TestServeConnection_RestoreSnapshotBroadcastsToOtherPeers(t *testing.T) {
	deps := newTestDeps(t)

	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("opening test project: %v", err)
	}

	target := filepath.Join(deps.defaultProjectRoot, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}
	snap, captured, err := pc.SnapStore.CapturePreMutation("turn-1", "test turn", pc.Root, target)
	if err != nil || !captured {
		t.Fatalf("CapturePreMutation: captured=%v err=%v", captured, err)
	}
	if err := os.WriteFile(target, []byte("package main\n\nfunc mutated() {}\n"), 0644); err != nil {
		t.Fatalf("mutating file: %v", err)
	}

	aOut := &bytes.Buffer{}
	aIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), aOut)
	deps.peers.Add(aIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-a", DeviceType: "desktop", Avatar: ""})

	bOut := &bytes.Buffer{}
	bIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), bOut)
	peerB := deps.peers.Add(bIPC, &fakeCloser{}, "unix", peerIdentity{Name: "peer-b", DeviceType: "desktop", Avatar: ""})

	line := envelopeJSON(t, protocol.EventRestoreSnapshot, protocol.RestoreSnapshotData{
		SnapshotID: snap.ID,
	})
	in := bytes.NewBufferString(line + "\n")
	reqOut := &bytes.Buffer{}
	reqIPC := protocol.NewIPCStream(in, reqOut)
	serveConnection(context.Background(), reqIPC, deps, "unix", peerB.ID, nil)

	if !strings.Contains(reqOut.String(), `"type":"snapshot_restored"`) {
		t.Fatalf("expected the requester to receive snapshot_restored, got: %s", reqOut.String())
	}
	if !strings.Contains(aOut.String(), `"type":"snapshot_restored"`) {
		t.Fatalf("expected the bystander peer to receive a broadcast snapshot_restored, got: %s", aOut.String())
	}
}
