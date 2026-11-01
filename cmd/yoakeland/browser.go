package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// browserBroker relays the agent's browser tool to a desktop client's browser
// panel (the t3code pattern: the browser the user already sees is the one the
// agent drives, rather than a hidden headless one). A client offers itself
// with browser_host; each action goes to exactly one host as a
// browser_request and waits for that host's browser_response. With no such
// client the action runs in the server's own browser (serverbrowser.go).
type browserBroker struct {
	peers *PeerRegistry
	// server is the server's own headless browser, used when no desktop client hosts one; nil
	// where Chrome can't run (Windows) or in tests.
	server *serverBrowser

	mu      sync.Mutex
	pending map[string]*browserCall
	// approved is, per chat, the hosts the agent has navigated to (each passed
	// the permission check). Kept while the daemon runs, like a session's
	// "allow this host" answer.
	approved map[string]map[string]bool
	// blockCrossOrigin is the runtime setting browserBlockCrossOrigin.
	blockCrossOrigin atomic.Bool
}

type browserCall struct {
	peerID string
	done   chan protocol.BrowserResponseData
}

// browserTimeout bounds one action, page load included.
const browserTimeout = 60 * time.Second

var errNoBrowserHost = errors.New("no browser available: the browser tool needs the yoake desktop app connected to this server (and this chat open, from another machine)")

func newBrowserBroker(peers *PeerRegistry) *browserBroker {
	b := &browserBroker{peers: peers, pending: map[string]*browserCall{}, approved: map[string]map[string]bool{}}
	b.blockCrossOrigin.Store(true)
	return b
}

// SetBlockCrossOrigin applies the browserBlockCrossOrigin runtime setting.
func (b *browserBroker) SetBlockCrossOrigin(on bool) {
	if b != nil {
		b.blockCrossOrigin.Store(on)
	}
}

// approve remembers the host of a navigate the permission check let through.
func (b *browserBroker) approve(sessionID, action string, args json.RawMessage) {
	if action != "navigate" {
		return
	}
	var a struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(args, &a) != nil {
		return
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Hostname() == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.approved[sessionID] == nil {
		b.approved[sessionID] = map[string]bool{}
	}
	b.approved[sessionID][strings.ToLower(u.Hostname())] = true
}

func (b *browserBroker) allowedHosts(sessionID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	hosts := make([]string, 0, len(b.approved[sessionID]))
	for h := range b.approved[sessionID] {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// Browse implements tool.BrowserRelay.
func (b *browserBroker) Browse(ctx context.Context, sessionID, action string, args json.RawMessage) (tool.BrowserResult, error) {
	if b == nil {
		return tool.BrowserResult{}, errNoBrowserHost
	}
	p, projectPath := b.peers.BrowserHostFor(sessionID)
	if p == nil {
		if b.server == nil {
			return tool.BrowserResult{}, errNoBrowserHost
		}
		b.approve(sessionID, action, args)
		ctx, cancel := context.WithTimeout(ctx, browserTimeout)
		defer cancel()
		res, err := b.server.Browse(ctx, sessionID, action, args, browserGuard{b.blockCrossOrigin.Load(), b.allowedHosts(sessionID)})
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("the browser didn't answer within %s", browserTimeout)
		}
		return res, err
	}
	call := &browserCall{peerID: p.ID, done: make(chan protocol.BrowserResponseData, 1)}
	reqID := "br-" + randomID()
	b.mu.Lock()
	b.pending[reqID] = call
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, reqID)
		b.mu.Unlock()
	}()

	b.approve(sessionID, action, args)
	req := protocol.BrowserRequestData{
		RequestID: reqID, SessionID: sessionID, ProjectPath: projectPath, Action: action, Args: args,
	}
	if b.blockCrossOrigin.Load() {
		req.BlockCrossOrigin, req.AllowedHosts = true, b.allowedHosts(sessionID)
	}
	err := p.ipc.SendEvent(protocol.EventBrowserRequest, sessionID, "", "", req)
	if err != nil {
		return tool.BrowserResult{}, fmt.Errorf("reaching the browser: %w", err)
	}
	select {
	case r := <-call.done:
		if r.Error != "" {
			return tool.BrowserResult{}, errors.New(r.Error)
		}
		return tool.BrowserResult{Output: r.Output, Image: r.Image}, nil
	case <-ctx.Done():
		return tool.BrowserResult{}, ctx.Err()
	case <-time.After(browserTimeout):
		return tool.BrowserResult{}, fmt.Errorf("the browser didn't answer within %s", browserTimeout)
	}
}

// Respond delivers a browser_response — only from the peer the request went
// to, so no other connection can answer (or spoof) it.
func (b *browserBroker) Respond(peerID string, r protocol.BrowserResponseData) {
	if b == nil {
		return
	}
	b.mu.Lock()
	call, ok := b.pending[r.RequestID]
	b.mu.Unlock()
	if ok && call.peerID == peerID {
		select {
		case call.done <- r:
		default:
		}
	}
}

// DropPeer fails whatever was waiting on a host that disconnected.
func (b *browserBroker) DropPeer(peerID string) {
	if b == nil {
		return
	}
	if b.server != nil {
		b.server.DropPeer(peerID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, call := range b.pending {
		if call.peerID == peerID {
			select {
			case call.done <- protocol.BrowserResponseData{RequestID: id, Error: "the browser's app disconnected"}:
			default:
			}
		}
	}
}

// serverEvent handles the browser_view/frame_ack/control/input/nav events of the server's browser.
func (b *browserBroker) serverEvent(ipc *protocol.IPCStream, peerID string, env *protocol.Envelope) {
	if b == nil || peerID == "" {
		return
	}
	if b.server == nil {
		if p, ok := b.peers.Get(peerID); ok && env.Type == protocol.EventBrowserView {
			_ = p.ipc.SendEvent(protocol.EventBrowserState, "", "", "", protocol.BrowserStateData{Error: errNoServerBrows.Error(), Tabs: []protocol.BrowserTabInfo{}})
		}
		return
	}
	b.server.handle(ipc, peerID, env)
}
