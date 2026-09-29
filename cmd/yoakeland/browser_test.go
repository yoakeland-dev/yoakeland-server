package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// browserPeer registers a peer and returns the browser_requests sent to it;
// every other frame (peer_list broadcasts) is drained so writes never block.
func browserPeer(t *testing.T, peers *PeerRegistry, network string) (*Peer, chan protocol.BrowserRequestData) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	reqs := make(chan protocol.BrowserRequestData, 8)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var env protocol.Envelope
			if json.Unmarshal(sc.Bytes(), &env) == nil && env.Type == protocol.EventBrowserRequest {
				var d protocol.BrowserRequestData
				_ = json.Unmarshal(env.Data, &d)
				reqs <- d
			}
		}
	}()
	return peers.Add(protocol.NewIPCStream(nil, w), nopCloser{}, network, peerIdentity{DeviceType: "desktop"}), reqs
}

func TestBrowserBroker_HostChoiceAndSpoofing(t *testing.T) {
	peers := NewPeerRegistry()
	b := newBrowserBroker(peers)
	ctx := context.Background()

	if _, err := b.Browse(ctx, "s1", "snapshot", nil); err != errNoBrowserHost {
		t.Fatalf("no host: %v", err)
	}
	remote, remoteSc := browserPeer(t, peers, "tcp")
	peers.SetBrowserHost(remote.ID, true)
	// A remote host viewing another chat must not get this one's traffic.
	if _, err := b.Browse(ctx, "s1", "snapshot", nil); err != errNoBrowserHost {
		t.Fatalf("remote host not viewing s1 was used: %v", err)
	}

	local, localSc := browserPeer(t, peers, "unix")
	peers.SetBrowserHost(local.ID, true)
	go func() {
		req := <-localSc
		b.Respond(remote.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "spoofed"})
		time.Sleep(20 * time.Millisecond)
		b.Respond(local.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "real"})
	}()
	if out, err := b.Browse(ctx, "s1", "snapshot", nil); err != nil || out != "real" {
		t.Fatalf("local host: %q, %v", out, err)
	}

	// A host with the chat open wins over the local fallback.
	peers.SetActivity(remote.ID, "s1", "")
	go func() {
		req := <-remoteSc
		b.Respond(remote.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Error: "boom"})
	}()
	if _, err := b.Browse(ctx, "s1", "snapshot", nil); err == nil || err.Error() != "boom" {
		t.Fatalf("viewing host: %v", err)
	}

	// A host dropping mid-request fails it instead of hanging.
	go func() {
		<-remoteSc
		b.DropPeer(remote.ID)
	}()
	if _, err := b.Browse(ctx, "s1", "snapshot", nil); err == nil {
		t.Fatal("dropped host's request succeeded")
	}
}

// scriptedBrowsing calls the browser tool once, then finishes.
type scriptedBrowsing struct{ rounds int }

func (s *scriptedBrowsing) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	out, errs := make(chan agent.ModelStreamChunk, 1), make(chan error)
	s.rounds++
	if s.rounds == 1 {
		out <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{
			ID: "b1", Name: "browser", Args: json.RawMessage(`{"action":"navigate","url":"http://localhost:3000"}`),
		}}
	} else {
		out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "looks fine"}
	}
	close(out)
	close(errs)
	return out, errs
}

// TestBrowserTool_RelaysThroughProject: a project's browser tool reaches the
// hosting client through the broker and the page text comes back to the
// model marked as untrusted.
func TestBrowserTool_RelaysThroughProject(t *testing.T) {
	peers := NewPeerRegistry()
	projects, err := NewProjectRegistry(&scriptedBrowsing{}, "p", 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects.browser = newBrowserBroker(peers)
	pc, err := projects.Get(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, reqs := browserPeer(t, peers, "unix")
	peers.SetBrowserHost(host.ID, true)
	go func() {
		req := <-reqs
		if req.Action != "navigate" || req.SessionID != "s1" {
			t.Errorf("request = %+v", req)
		}
		projects.browser.Respond(host.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "Title: Home"})
	}()

	ag := pc.Registry.Get(protocol.NewIPCStream(nil, io.Discard), "s1")
	ag.SetMode(agent.ModeAutonomous)
	if err := ag.RunTurn(context.Background(), "s1", "t1", "check the page"); err != nil {
		t.Fatal(err)
	}
	state, _ := pc.SessStore.Load("s1")
	for _, m := range state.History {
		if m.Role == "tool" {
			if !strings.Contains(m.Content, "Title: Home") || !strings.Contains(m.Content, "untrusted") {
				t.Fatalf("browser result = %q", m.Content)
			}
			return
		}
	}
	t.Fatal("no browser tool result")
}
