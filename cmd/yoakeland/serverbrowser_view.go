package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	"slices"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/protocol"
)

// The server browser's people: who is watching (screencast frames, one in flight per viewer), who
// may drive it (a single controller seat with an epoch), and the browser_* events that say so.

const (
	frameAckPace     = 50 * time.Millisecond // Chromium is acked this long after a frame, so ~20 fps at most
	viewerAckTimeout = 3 * time.Second       // a viewer that never acks gets the next frame anyway
	maxFrameData     = 700_000               // base64 bytes; a bigger JPEG is dropped and quality lowered
	inputRate        = 120                   // operate events per second per peer
)

type frame struct {
	seq  uint64
	data string
	w, h int
}

// viewer is one peer watching. Only one frame is on the wire at a time; frames arriving meanwhile
// replace each other, so a slow peer sees the newest picture rather than a growing backlog.
type viewer struct {
	peerID    string
	sessionID string // the chat this viewer's panel belongs to, for tabs it creates
	w, h      int
	send      func(*frame) error

	mu       sync.Mutex
	inflight uint64
	sentAt   time.Time
	pending  *frame
}

func (v *viewer) offer(f *frame) {
	v.mu.Lock()
	if v.inflight != 0 && time.Since(v.sentAt) < viewerAckTimeout {
		v.pending = f
		v.mu.Unlock()
		return
	}
	v.inflight, v.sentAt, v.pending = f.seq, time.Now(), nil
	v.mu.Unlock()
	go v.write(f)
}

func (v *viewer) ack(seq uint64) {
	v.mu.Lock()
	if seq != v.inflight {
		v.mu.Unlock()
		return
	}
	f := v.pending
	v.inflight, v.pending = 0, nil
	if f != nil {
		v.inflight, v.sentAt = f.seq, time.Now()
	}
	v.mu.Unlock()
	if f != nil {
		go v.write(f)
	}
}

func (v *viewer) write(f *frame) {
	if v.send(f) != nil {
		v.mu.Lock()
		if v.inflight == f.seq {
			v.inflight = 0
		}
		v.mu.Unlock()
	}
}

type cast struct {
	sid     string
	w, h, q int
}

type control struct {
	who   string // "", "agent" or "human"
	peer  string // the human's peer
	epoch uint64 // bumped on every change of hands
}

type bucket struct {
	tokens float64
	last   time.Time
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// ---- screencast ----

func (s *serverBrowser) viewerList() []*viewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		out = append(out, v)
	}
	return out
}

// reconcileCast makes the one running screencast match what is wanted: the active tab at the
// current size, while anyone watches. A freshly started one is followed by a still, since a page
// that is not repainting sends no frame.
func (s *serverBrowser) reconcileCast() {
	s.castMu.Lock()
	defer s.castMu.Unlock()
	s.mu.Lock()
	var want *cast
	if s.br != nil && s.active != nil && len(s.viewers) > 0 {
		if s.qualSid != s.active.sid { // another tab: its pages may compress differently
			s.quality, s.shrink, s.qualSid = 60, 100, s.active.sid
		}
		want = &cast{s.active.sid, s.vp.w * s.shrink / 100, s.vp.h * s.shrink / 100, s.quality}
	}
	cur := s.cast
	if (want == nil && cur == nil) || (want != nil && cur != nil && *want == *cur) {
		s.mu.Unlock()
		return
	}
	s.cast = want
	s.mu.Unlock()
	if cur != nil {
		_ = s.bg(cur.sid, "Page.stopScreencast", nil)
	}
	if want != nil {
		_ = s.bg(want.sid, "Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": want.q, "maxWidth": want.w, "maxHeight": want.h, "everyNthFrame": 1,
		})
		s.still(s.viewerList())
	}
}

// still sends the active tab's current picture to vs.
func (s *serverBrowser) still(vs []*viewer) {
	t := s.activeTab()
	if t == nil || len(vs) == 0 {
		return
	}
	s.mu.Lock()
	q, shrink, vp := s.quality, s.shrink, s.vp
	s.mu.Unlock()
	params := map[string]any{"format": "jpeg", "quality": q}
	if shrink < 100 {
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": vp.w, "height": vp.h, "scale": float64(shrink) / 100}
	}
	var shot struct {
		Data string `json:"data"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.do(ctx, t.sid, "Page.captureScreenshot", params, &shot) != nil || len(shot.Data) > maxFrameData {
		return
	}
	s.mu.Lock()
	s.seq++
	f := &frame{seq: s.seq, data: shot.Data}
	f.w, f.h = jpegSize(shot.Data, s.vp.w, s.vp.h)
	s.mu.Unlock()
	for _, v := range vs {
		v.offer(f)
	}
}

// onFrame runs on the read loop. Chromium is acked on a timer for every frame, whether or not any
// viewer could take it, so it never stalls.
func (s *serverBrowser) onFrame(sid string, raw json.RawMessage) {
	var p struct {
		Data      string `json:"data"`
		SessionID int    `json:"sessionId"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	time.AfterFunc(frameAckPace, func() {
		_ = s.bg(sid, "Page.screencastFrameAck", map[string]any{"sessionId": p.SessionID})
	})
	s.mu.Lock()
	c := s.cast
	if c == nil || c.sid != sid {
		s.mu.Unlock()
		return
	}
	if len(p.Data) > maxFrameData {
		// Lower the quality first; at its floor, shrink the picture (input is scaled back up).
		if s.quality > 30 {
			s.quality -= 15
		} else if s.shrink > 30 {
			s.shrink -= 20
		}
		s.mu.Unlock()
		go s.reconcileCast()
		return
	}
	s.seq++
	f := &frame{seq: s.seq, data: p.Data}
	f.w, f.h = jpegSize(p.Data, c.w, c.h)
	vs := make([]*viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		vs = append(vs, v)
	}
	s.mu.Unlock()
	for _, v := range vs {
		v.offer(f)
	}
}

// jpegSize reads a base64 JPEG's dimensions from its header, falling back to w, h.
func jpegSize(b64 string, w, h int) (int, int) {
	cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64)))
	if err != nil {
		return w, h
	}
	return cfg.Width, cfg.Height
}

func (s *serverBrowser) setViewport(w, h int) {
	s.mu.Lock()
	if s.vp == (viewport{w, h}) {
		s.mu.Unlock()
		return
	}
	s.vp = viewport{w, h}
	tabs, mobile := slices.Clone(s.tabs), s.dev.mobile
	s.mu.Unlock()
	for _, t := range tabs {
		_ = s.bg(t.sid, "Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": 1, "mobile": mobile})
	}
	s.reconcileCast()
}

// ---- state ----

func (s *serverBrowser) pushState() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// stateLoop sends browser_state to every peer after any change, once the browser has been used.
func (s *serverBrowser) stateLoop() {
	for range s.dirty {
		s.mu.Lock()
		used := s.used
		s.mu.Unlock()
		if !used {
			continue
		}
		for _, p := range s.peers.All() {
			s.sendStateTo(p)
		}
	}
}

func (s *serverBrowser) canOperate(p *Peer) bool {
	return p.Network == "unix" || !s.peers.IsRestricted(p.ID)
}

func (s *serverBrowser) stateFor(p *Peer) protocol.BrowserStateData {
	s.reapSeat()
	operate := s.canOperate(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := protocol.BrowserStateData{
		Available: s.supports && s.bootErr == "", Error: s.bootErr, Tabs: []protocol.BrowserTabInfo{},
		Controller: s.ctl.who, YouControl: s.ctl.who == "human" && s.ctl.peer == p.ID, CanOperate: operate,
	}
	if !s.supports {
		st.Error = errNoServerBrows.Error()
	}
	for _, t := range s.tabs {
		st.Tabs = append(st.Tabs, protocol.BrowserTabInfo{Title: t.title, URL: t.url, Active: t == s.active, Loading: t.loading})
	}
	return st
}

func (s *serverBrowser) sendStateTo(p *Peer) {
	_ = p.ipc.SendEvent(protocol.EventBrowserState, "", "", "", s.stateFor(p))
}

// ---- control ----

// humanSeatIdle is how long a person may hold the seat without input before it is taken back, so
// a forgotten panel can't lock the agent out.
const humanSeatIdle = 5 * time.Minute

// reapSeat frees the seat if its holder is gone, was demoted to watch-only, or has gone idle.
func (s *serverBrowser) reapSeat() {
	s.mu.Lock()
	who, peer, idle := s.ctl.who, s.ctl.peer, time.Since(s.ctlAt) > humanSeatIdle
	s.mu.Unlock()
	if who != "human" {
		return
	}
	if p, ok := s.peers.Get(peer); !ok || !s.canOperate(p) || idle {
		_ = s.setControl(peer, false)
	}
}

// beginAgent seats the agent for one action. end hands the seat back and reports whether a human
// took it meanwhile, and the first navigation the guard stopped.
func (s *serverBrowser) beginAgent(ctx context.Context, g browserGuard) (context.Context, uint64, func(uint64) (string, bool), error) {
	s.reapSeat()
	s.mu.Lock()
	if s.ctl.who == "human" {
		s.mu.Unlock()
		return nil, 0, nil, errHumanHasIt
	}
	actx, cancel := context.WithCancel(ctx)
	s.ctl = control{who: "agent", epoch: s.ctl.epoch + 1}
	epoch := s.ctl.epoch
	s.actCancel = cancel
	s.lastUse = time.Now()
	gd := &navGuard{on: g.on, hosts: map[string]bool{}}
	for _, h := range g.hosts {
		gd.hosts[strings.ToLower(h)] = true
	}
	s.guard = gd
	s.mu.Unlock()
	s.pushState()
	return actx, epoch, func(ep uint64) (string, bool) {
		cancel()
		s.mu.Lock()
		taken := s.ctl.epoch != ep || s.ctl.who != "agent"
		if !taken {
			s.ctl = control{epoch: ep + 1}
		}
		s.guard, s.actCancel, s.lastUse = nil, nil, time.Now()
		blocked := ""
		if len(gd.blocked) > 0 {
			blocked = gd.blocked[0]
		}
		s.mu.Unlock()
		s.pushState()
		return blocked, taken
	}, nil
}

// setControl is a human taking or releasing the seat. Taking it from the agent ends the action
// in flight; releasing lets go of whatever the human still held down.
func (s *serverBrowser) setControl(peer string, take bool) error {
	if take {
		s.reapSeat()
	}
	s.mu.Lock()
	var release []cdpCmd
	sid := ""
	var vw, vh int
	switch {
	case take && s.ctl.who == "human":
		holder := s.ctl.peer
		s.mu.Unlock()
		if holder != peer {
			return errors.New("someone else has control of the browser")
		}
		return nil
	case take:
		if s.ctl.who == "agent" && s.actCancel != nil {
			s.actCancel()
		}
		s.ctl = control{who: "human", peer: peer, epoch: s.ctl.epoch + 1}
		s.used = true // so everyone is told, even before the browser has launched
		if v := s.viewers[peer]; v != nil {
			vw, vh = v.w, v.h
		}
	case s.ctl.who == "human" && s.ctl.peer == peer:
		s.ctl = control{epoch: s.ctl.epoch + 1}
		release = s.pressed.releaseAll()
		if s.active != nil {
			sid = s.active.sid
		}
	default:
		s.mu.Unlock()
		return nil
	}
	s.lastUse, s.ctlAt = time.Now(), time.Now()
	s.mu.Unlock()
	if len(release) > 0 { // never queued: a full queue must not leave a button down
		go s.runJob(inputJob{sid, release})
	}
	if vw > 0 {
		s.setViewport(vw, vh)
	}
	s.pushState()
	return nil
}

// DropPeer forgets a disconnected peer: it stops watching and, if it held the seat, lets go.
func (s *serverBrowser) DropPeer(peer string) {
	s.mu.Lock()
	_, watching := s.viewers[peer]
	delete(s.viewers, peer)
	delete(s.rate, peer)
	delete(s.viewRate, peer)
	s.mu.Unlock()
	_ = s.setControl(peer, false)
	if watching {
		s.reconcileCast()
	}
}

func (s *serverBrowser) allow(peer string) bool { return s.take(s.rate, peer, inputRate, inputRate) }

// allowView limits browser_view on/off, each of which can restart the screencast.
func (s *serverBrowser) allowView(peer string) bool { return s.take(s.viewRate, peer, 10, 4) }

// take spends a token from peer's bucket in m (burst tokens, refilled perSec a second).
func (s *serverBrowser) take(m map[string]*bucket, peer string, burst, perSec float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := m[peer]
	now := time.Now()
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		m[peer] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *serverBrowser) inputLoop() {
	for job := range s.input {
		s.runJob(job)
	}
}

func (s *serverBrowser) runJob(job inputJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, c := range job.cmds {
		if s.do(ctx, job.sid, c.Method, c.Params, nil) != nil {
			return
		}
	}
}

// ---- events ----

func (s *serverBrowser) onView(peerID string, d protocol.BrowserViewData) {
	p, ok := s.peers.Get(peerID)
	if !ok || !s.allowView(peerID) {
		return
	}
	if !d.On {
		s.mu.Lock()
		delete(s.viewers, peerID)
		s.mu.Unlock()
		s.reconcileCast()
		return
	}
	operator := s.canOperate(p)
	w, h := clamp(d.Width, minVPW, maxVPDim), clamp(d.Height, minVPH, maxVPDim)
	s.mu.Lock()
	v := s.viewers[peerID]
	fresh := v == nil
	if fresh {
		v = &viewer{peerID: peerID, send: func(f *frame) error {
			return p.ipc.SendEvent(protocol.EventBrowserFrame, "", "", "", protocol.BrowserFrameData{Seq: f.seq, Data: f.data, Width: f.w, Height: f.h})
		}}
		s.viewers[peerID] = v
		s.quality, s.shrink = 60, 100 // a new viewer may manage what the last one couldn't
	}
	v.w, v.h, v.sessionID = w, h, d.SessionID
	// Only the controller, or an operator watching alone, resizes the page, never mid-action.
	sizes := s.ctl.who != "agent" && ((s.ctl.who == "human" && s.ctl.peer == peerID) || (operator && len(s.viewers) == 1))
	s.lastUse = time.Now()
	bootable := operator && s.supports && s.br == nil && s.boot == nil && s.bootErr == ""
	s.mu.Unlock()
	if sizes {
		s.setViewport(w, h)
	}
	s.sendStateTo(p)
	if bootable { // a person at the panel wants a browser: start it rather than leave them a dead canvas
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = s.ensure(ctx)
		}()
	}
	s.reconcileCast()
	if fresh {
		s.still([]*viewer{v})
	}
}

func (s *serverBrowser) onAck(peerID string, seq uint64) {
	s.mu.Lock()
	v := s.viewers[peerID]
	s.mu.Unlock()
	if v != nil {
		v.ack(seq)
	}
}

// handle runs a browser_* event from peerID. View and ack are open to every peer; the rest
// operate the browser, which a restricted peer may not (remote_scope.go already refuses it; this
// is the second lock).
func (s *serverBrowser) handle(ipc *protocol.IPCStream, peerID string, env *protocol.Envelope) {
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: msg})
	}
	switch env.Type {
	case protocol.EventBrowserView:
		var d protocol.BrowserViewData
		if json.Unmarshal(env.Data, &d) == nil {
			s.onView(peerID, d)
		}
		return
	case protocol.EventBrowserFrameAck:
		var d protocol.BrowserFrameAckData
		if json.Unmarshal(env.Data, &d) == nil {
			s.onAck(peerID, d.Seq)
		}
		return
	}
	p, ok := s.peers.Get(peerID)
	if !ok || !s.canOperate(p) {
		fail("the server's browser can't be operated from this connection")
		return
	}
	if !s.allow(peerID) {
		return
	}
	switch env.Type {
	case protocol.EventBrowserControl:
		var d protocol.BrowserControlData
		if json.Unmarshal(env.Data, &d) == nil {
			if err := s.setControl(peerID, d.Take); err != nil {
				fail(err.Error())
			}
		}
	case protocol.EventBrowserInput:
		var d protocol.BrowserInputData
		if json.Unmarshal(env.Data, &d) == nil {
			s.input1(peerID, d)
		}
	case protocol.EventBrowserNav:
		var d protocol.BrowserNavData
		if json.Unmarshal(env.Data, &d) == nil {
			go func() {
				if err := s.humanNav(peerID, d); err != nil {
					fail(err.Error())
				}
			}()
		}
	}
}

// input1 queues one input event from the controller; anyone else's is dropped.
func (s *serverBrowser) input1(peerID string, in protocol.BrowserInputData) {
	s.mu.Lock()
	if s.ctl.who != "human" || s.ctl.peer != peerID || s.active == nil {
		s.mu.Unlock()
		return
	}
	s.pressed.vpW, s.pressed.vpH, s.pressed.scale = s.vp.w, s.vp.h, 100/float64(s.shrink)
	cmds, err := s.pressed.apply(in)
	sid := s.active.sid
	s.lastUse, s.ctlAt = time.Now(), time.Now()
	s.mu.Unlock()
	if err != nil {
		return
	}
	select {
	case s.input <- inputJob{sid, cmds}:
	default: // ponytail: a saturated queue drops events; the controller's next move catches up
	}
}

// humanNav is a navigation or tab command from an operator. It needs the seat to be theirs or
// free; with the agent mid-action it refuses rather than queue behind it.
func (s *serverBrowser) humanNav(peerID string, n protocol.BrowserNavData) error {
	s.reapSeat()
	s.mu.Lock()
	who, holder := s.ctl.who, s.ctl.peer
	owner := ""
	if v := s.viewers[peerID]; v != nil {
		owner = v.sessionID // tabs this person makes belong to the chat they're looking at
	}
	s.mu.Unlock()
	switch {
	case who == "agent":
		return errors.New("the agent is using the browser; take control first")
	case who == "human" && holder != peerID:
		return errors.New("someone else has control of the browser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.ensure(ctx); err != nil {
		return err
	}
	select {
	case s.act <- struct{}{}:
		defer func() { <-s.act }()
	case <-time.After(5 * time.Second):
		return errors.New("the browser is busy; try again")
	}
	t := s.activeTab()
	if t == nil {
		return errors.New("the browser isn't ready yet; try again")
	}
	var err error
	switch n.Action {
	case "navigate":
		if err = checkNavURL(n.URL); err != nil {
			return err
		}
		if t.owner != owner { // a tab keeps its owner: the person's page goes in a tab of their chat's own
			if t, err = s.newTab(ctx, owner); err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.navBy, s.navAt = peerID, time.Now()
		s.mu.Unlock()
		_ = s.bg(t.sid, "Page.stopLoading", nil) // a new address replaces a load still hanging
		// The seat isn't held for the whole load: after a moment it is the panel's loading
		// indicator that tells the person how it went.
		nctx, ncancel := context.WithTimeout(ctx, 3*time.Second)
		defer ncancel()
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		err := s.do(nctx, t.sid, "Page.navigate", map[string]any{"url": n.URL}, &nav)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err == nil && nav.ErrorText != "" && !strings.Contains(nav.ErrorText, "ERR_ABORTED") {
			err = errors.New("couldn't load the page: " + strings.TrimPrefix(nav.ErrorText, "net::") + " (" + n.URL + ")")
		}
		return err
	case "back", "forward":
		return s.history(ctx, t, n.Action)
	case "reload":
		return s.do(ctx, t.sid, "Page.reload", nil, nil)
	case "tab_new":
		_, err := s.newTab(ctx, owner)
		return err
	case "tab_select", "tab_close":
		target, err := s.tabAt(n.Index, anyOwner)
		if err != nil {
			return err
		}
		if n.Action == "tab_select" {
			s.selectTab(target)
			return nil
		}
		return s.closeTab(ctx, target)
	}
	return errors.New("unknown browser action")
}
