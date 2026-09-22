package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/protocol"
)

// TestServeConnection_SessionUpdateBroadcastsToOtherPeers guards the
// multi-connection sync fix: a mutation made on one connection (renaming a
// session here) must reach every other connection currently attached to
// this server process live, not just the requester — this is what makes a
// chat created/edited/renamed on one connection (e.g. the local unix
// socket) show up on another (e.g. a TCP peer) without that second
// connection having to poll.
func TestServeConnection_SessionUpdateBroadcastsToOtherPeers(t *testing.T) {
	deps := newTestDeps(t)

	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatalf("opening test project: %v", err)
	}
	if err := pc.SessStore.Ensure("sess-broadcast"); err != nil {
		t.Fatalf("creating session: %v", err)
	}

	// Peer A: a bystander connection that never sends anything itself, just
	// listens — its own output buffer is what a live broadcast would land
	// in.
	aOut := &bytes.Buffer{}
	aIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), aOut)
	deps.peers.Add(aIPC, &fakeCloser{}, "unix", "peer-a", "desktop", "")

	// Peer B: the requester, on a separate connection/peer.
	bOut := &bytes.Buffer{}
	bIPC := protocol.NewIPCStream(bytes.NewBuffer(nil), bOut)
	peerB := deps.peers.Add(bIPC, &fakeCloser{}, "unix", "peer-b", "desktop", "")

	line := envelopeJSON(t, protocol.EventSetSessionTitle, protocol.SetSessionTitleData{
		SessionID: "sess-broadcast", Title: "renamed from B",
	})
	in := bytes.NewBufferString(line + "\n")
	reqOut := &bytes.Buffer{}
	reqIPC := protocol.NewIPCStream(in, reqOut)
	serveConnection(context.Background(), reqIPC, deps, "unix", peerB.ID, nil)

	if !strings.Contains(aOut.String(), `"type":"session_updated"`) {
		t.Fatalf("expected the bystander peer to receive a broadcast session_updated, got: %s", aOut.String())
	}
	var env protocol.Envelope
	for _, line := range strings.Split(strings.TrimSpace(aOut.String()), "\n") {
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("malformed broadcast output %q: %v", line, err)
		}
		if env.Type == protocol.EventSessionUpdated {
			break
		}
	}
	var summary protocol.SessionSummary
	if err := json.Unmarshal(env.Data, &summary); err != nil {
		t.Fatalf("malformed session_updated data: %v", err)
	}
	if summary.Title != "renamed from B" {
		t.Fatalf("expected the broadcast to carry the new title, got %+v", summary)
	}

	if strings.Contains(bOut.String(), `"type":"session_updated"`) {
		t.Fatalf("peer B never sent anything on its own registered connection — it shouldn't receive its own broadcast there, got: %s", bOut.String())
	}
}
