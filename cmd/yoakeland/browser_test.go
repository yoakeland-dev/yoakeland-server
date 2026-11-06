package main

import (
	"bufio"
	"bytes"
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
	// With nothing local, a full-access remote host serves a chat it isn't viewing.
	go func() {
		req := <-remoteSc
		b.Respond(remote.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "remote"})
	}()
	if out, err := b.Browse(ctx, "s1", "snapshot", nil); err != nil || out.Output != "remote" {
		t.Fatalf("remote fallback: %+v, %v", out, err)
	}

	local, localSc := browserPeer(t, peers, "unix")
	peers.SetBrowserHost(local.ID, true)
	go func() {
		req := <-localSc
		b.Respond(remote.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "spoofed"})
		time.Sleep(20 * time.Millisecond)
		b.Respond(local.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "real"})
	}()
	if out, err := b.Browse(ctx, "s1", "snapshot", nil); err != nil || out.Output != "real" {
		t.Fatalf("local host: %+v, %v", out, err)
	}

	// A host with the chat open wins over the fallbacks.
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
	projects, err := NewProjectRegistry(&scriptedBrowsing{}, "p", 0, t.TempDir(), "")
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

// Every request carries the hosts this chat's agent navigated to, so the
// desktop panel can refuse to follow a link anywhere else.
func TestBrowserBroker_SendsApprovedHosts(t *testing.T) {
	peers := NewPeerRegistry()
	b := newBrowserBroker(peers)
	host, reqs := browserPeer(t, peers, "unix")
	peers.SetBrowserHost(host.ID, true)
	do := func(sessionID, action, args string) protocol.BrowserRequestData {
		t.Helper()
		go func() {
			req := <-reqs
			b.Respond(host.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "ok"})
			reqs <- req // hand the request back to the caller
		}()
		if _, err := b.Browse(context.Background(), sessionID, action, json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
		return <-reqs
	}

	if r := do("s1", "snapshot", `{}`); !r.BlockCrossOrigin || len(r.AllowedHosts) != 0 {
		t.Fatalf("before any navigate: %+v", r)
	}
	do("s1", "navigate", `{"url":"https://Docs.Example.com/x"}`)
	r := do("s1", "click", `{"ref":"3"}`)
	if len(r.AllowedHosts) != 1 || r.AllowedHosts[0] != "docs.example.com" {
		t.Fatalf("hosts = %v", r.AllowedHosts)
	}
	if r := do("s2", "snapshot", `{}`); len(r.AllowedHosts) != 0 {
		t.Fatalf("another chat inherited hosts: %v", r.AllowedHosts)
	}
	b.SetBlockCrossOrigin(false)
	if r := do("s1", "click", `{"ref":"3"}`); r.BlockCrossOrigin || len(r.AllowedHosts) != 0 {
		t.Fatalf("setting off but still enforcing: %+v", r)
	}
}

// The browser goes to a host that has the chat open, else to the newest local host, else to a
// full-access remote one; a restricted device never hosts.
func TestBrowserHostFor(t *testing.T) {
	reg := NewPeerRegistry()
	add := func(network string) *Peer {
		return reg.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, network, peerIdentity{Name: network})
	}
	remote := add("tcp")
	reg.SetBrowserHost(remote.ID, true)
	reg.SetActivity(remote.ID, "chat", "/remote/proj")
	if p, _ := reg.BrowserHostFor("chat"); p != remote {
		t.Fatal("the host with the chat open was not chosen")
	}
	local := add("unix")
	reg.SetBrowserHost(local.ID, true)
	reg.SetActivity(local.ID, "", "/local/proj")
	if p, project := reg.BrowserHostFor("other"); p != local || project != "/local/proj" {
		t.Fatalf("got %v %q, want the local host", p, project)
	}
	if p, _ := reg.BrowserHostFor("unknown"); p != local {
		t.Fatal("a remote host beat the local one")
	}
	reg.SetBrowserHost(local.ID, false)
	if p, _ := reg.BrowserHostFor("unknown"); p != remote {
		t.Fatal("the only host, a full-access remote one, was not used")
	}
	reg.SyncAccess(func(string) (string, string, string) { return TierLimited, "u", "" })
	if p, _ := reg.BrowserHostFor("unknown"); p != nil {
		t.Fatal("a restricted device was chosen to host")
	}
}
