package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/chrome"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// serverBrowser is the agent's browser when no desktop client hosts one: a headless Chrome on the
// server (internal/chrome) with tabs like the desktop panel, driven over CDP. It is installed and
// launched on first use, relaunched after a crash and closed after browserIdle with nobody
// watching. Agent actions run one at a time; humans watch, and with the right scope drive it,
// through browser_view/browser_input/... (serverbrowser_view.go).
type serverBrowser struct {
	peers    *PeerRegistry
	stateDir string
	ports    func() []uint16 // yoakeland's own listeners, which the browser may not reach
	launch   func(ctx context.Context, blocked []uint16, progress func(done, total int64)) (cdp, error)
	idle     time.Duration
	supports bool // false where Chrome can't be launched (Windows)

	act   chan struct{} // held by the agent action (or human navigation) in progress
	dirty chan struct{} // browser_state needs sending
	input chan inputJob // human input, executed in order off the dispatch goroutine

	mu        sync.Mutex
	br        cdp
	boot      *bootState
	bootErr   string
	used      bool
	launched  []uint16
	lastUse   time.Time
	tabs      []*tab
	active    *tab
	byTarget  map[string]*tab
	bySession map[string]*tab
	vp        viewport
	dev       device // the emulated device's mobile flag and user agent; w/h live in vp
	ua        string // the browser's own user agent without "Headless", which many sites turn away
	guard     *navGuard
	actCancel context.CancelFunc
	navBy     string // the peer whose navigation is loading, told if it is refused
	navAt     time.Time
	ctlAt     time.Time // the controller's last input
	shrink    int       // percent of the viewport the screencast is scaled to (lowered when frames stay too big)
	qualSid   string    // the tab quality and shrink were tuned for

	viewers  map[string]*viewer
	rate     map[string]*bucket
	viewRate map[string]*bucket
	ctl      control
	pressed  inputState
	cast     *cast
	castMu   sync.Mutex
	quality  int
	seq      uint64
}

// cdp is what the backend needs of *chrome.Browser; tests substitute a fake.
type cdp interface {
	Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error)
	Subscribe(fn func(sessionID, method string, params json.RawMessage)) (cancel func())
	Done() <-chan struct{}
	Close()
}

type viewport struct{ w, h int }

type tab struct {
	targetID, sid string
	url, title    string
	loading       bool
	err           string    // why the last navigation failed
	console       []string  // warnings and errors since the last snapshot
	owner         string    // the chat whose agent may see and act on this tab
	net           []*netReq // requests since the last navigation, newest last (maxNetReqs)
	netByID       map[string]*netReq
	popup         bool // a window the page opened: never listed, its first load is refused
}

// netReq is one request as the network action lists it.
type netReq struct {
	id, method, url, kind, errText string
	status                         int
	size                           int64
	done                           bool
}

// device is an emulate preset. Every preset renders at device scale 1: frames, clicks and
// screenshots all work in CSS pixels, and a sharper picture isn't worth the extra bytes.
type device struct {
	w, h   int
	mobile bool
	ua     string
}

var devices = map[string]device{
	"iphone":  {390, 844, true, "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"},
	"ipad":    {820, 1180, true, "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"},
	"android": {412, 915, true, "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"},
	"desktop": {defaultVPW, defaultVPH, false, ""},
}

type bootState struct {
	done     chan struct{}
	err      error
	got, all int64 // install progress, bytes (guarded by serverBrowser.mu)
}

// navGuard is the cross-origin guard of one agent action: main-frame navigations to hosts outside
// hosts (and loopback) are stopped and remembered.
type navGuard struct {
	on      bool
	hosts   map[string]bool
	blocked []string // messages, first one is reported
}

type browserGuard struct {
	on    bool
	hosts []string
}

// browserBootWait is how long an action waits for the install and launch before reporting progress.
var browserBootWait = 45 * time.Second

const (
	browserIdle      = 10 * time.Minute
	browserShotWidth = 1280
	defaultVPW       = 1280
	defaultVPH       = 800
	minVPW, minVPH   = 320, 240 // the viewer's size clamp, browser_view in SPEC.md
	maxVPDim         = 1920
	maxConsoleLines  = 30
	maxTabs          = 16
	maxOwnerTabs     = 6
	maxNetReqs       = 300
	maxShotHeight    = 10000 // CSS px; a longer page is cut here
)

var (
	errBrowserGone   = errors.New("the browser isn't running; try again")
	errHumanHasIt    = errors.New("the user has taken control of the browser; wait for them to release it")
	errNoServerBrows = errors.New("the server's browser isn't available here")
)

//go:embed serverbrowser.js
var browserScripts string

var jsSections = func() map[string]string {
	out := map[string]string{}
	var name string
	var b strings.Builder
	flush := func() {
		if name != "" {
			out[name] = strings.TrimSpace(b.String())
		}
		b.Reset()
	}
	for _, line := range strings.Split(browserScripts, "\n") {
		if n, ok := strings.CutPrefix(line, "//== "); ok {
			flush()
			name = n
		} else if name != "" {
			b.WriteString(line + "\n")
		}
	}
	flush()
	return out
}()

var plainRef = regexp.MustCompile(`[^\w-]`)

// js fills a script section's placeholders in one pass, so a value can't smuggle in another.
func js(section string, kv ...string) string {
	return strings.NewReplacer(kv...).Replace(jsSections[section])
}

func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func newServerBrowser(peers *PeerRegistry, stateDir string, ports func() []uint16) *serverBrowser {
	s := &serverBrowser{
		peers: peers, stateDir: stateDir, ports: ports, idle: browserIdle, supports: true,
		act: make(chan struct{}, 1), dirty: make(chan struct{}, 1), input: make(chan inputJob, 256),
		byTarget: map[string]*tab{}, bySession: map[string]*tab{},
		vp: viewport{defaultVPW, defaultVPH}, viewers: map[string]*viewer{}, rate: map[string]*bucket{}, viewRate: map[string]*bucket{},
		quality: 60, shrink: 100,
	}
	s.launch = s.chromeLaunch
	go s.stateLoop()
	go s.inputLoop()
	go s.idleLoop()
	return s
}

func (s *serverBrowser) chromeLaunch(ctx context.Context, blocked []uint16, progress func(done, total int64)) (cdp, error) {
	bin, err := chrome.Ensure(ctx, s.stateDir, progress)
	if err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	b, err := chrome.Launch(lctx, chrome.Options{Binary: bin, StateDir: s.stateDir, BlockedPorts: blocked})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ---- lifecycle ----

func (s *serverBrowser) browser() cdp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.br
}

func closed(b cdp) bool {
	select {
	case <-b.Done():
		return true
	default:
		return false
	}
}

// ensure returns once Chrome is running with a tab, installing and launching it first if need be.
// An install slower than browserBootWait is reported as an error while it carries on.
func (s *serverBrowser) ensure(ctx context.Context) error {
	if !s.supports {
		return errNoServerBrows
	}
	for {
		ports := s.ports()
		s.mu.Lock()
		s.used, s.lastUse = true, time.Now()
		if b := s.br; b != nil {
			if !closed(b) && slices.Equal(s.launched, ports) {
				s.mu.Unlock()
				return nil
			}
			s.mu.Unlock()
			b.Close() // dead, or yoakeland's listeners moved
			s.teardown(b)
			continue
		}
		st := s.boot
		if st == nil {
			st = &bootState{done: make(chan struct{})}
			s.boot = st
			go s.runBoot(st)
		}
		s.mu.Unlock()
		select {
		case <-st.done:
			if st.err != nil {
				return st.err
			}
		case <-time.After(browserBootWait):
			s.mu.Lock()
			got, all := st.got, st.all
			s.mu.Unlock()
			if all > 0 && got < all {
				return fmt.Errorf("installing Chrome (%d of %d MB); try again in a minute", got>>20, all>>20)
			}
			return errors.New("the browser is still starting; try again in a minute")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *serverBrowser) runBoot(st *bootState) {
	blocked := s.ports()
	b, err := s.launch(context.Background(), blocked, func(done, total int64) {
		s.mu.Lock()
		st.got, st.all = done, total
		s.mu.Unlock()
	})
	if err == nil {
		s.mu.Lock()
		s.br, s.launched = b, blocked
		s.mu.Unlock()
		b.Subscribe(func(sid, method string, p json.RawMessage) { s.onEvent(b, sid, method, p) })
		go func() { <-b.Done(); s.teardown(b) }()
		if err = s.setup(b); err != nil {
			b.Close()
		}
	}
	s.mu.Lock()
	if err != nil {
		s.bootErr = err.Error()
	} else {
		s.bootErr = ""
	}
	s.boot, st.err = nil, err
	s.mu.Unlock()
	close(st.done)
	s.pushState()
}

// setup locks the browser down and makes sure it has a tab.
func (s *serverBrowser) setup(b cdp) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call := func(method string, params any) error { _, err := b.Call(ctx, "", method, params); return err }
	_ = call("Browser.setDownloadBehavior", map[string]any{"behavior": "deny"})
	for _, p := range deniedPermissions {
		_ = call("Browser.setPermission", map[string]any{"permission": map[string]any{"name": p}, "setting": "denied"})
	}
	if raw, err := b.Call(ctx, "", "Browser.getVersion", nil); err == nil {
		var v struct {
			UserAgent string `json:"userAgent"`
		}
		if json.Unmarshal(raw, &v) == nil {
			s.mu.Lock()
			s.ua = strings.ReplaceAll(v.UserAgent, "HeadlessChrome", "Chrome")
			s.mu.Unlock()
		}
	}
	if err := call("Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		return err
	}
	if err := call("Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []map[string]any{{"type": "page"}, {"exclude": true}},
	}); err != nil {
		return err
	}
	raw, err := b.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return err
	}
	var targets struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	_ = json.Unmarshal(raw, &targets)
	made := false
	for _, ti := range targets.TargetInfos {
		if ti.Type != "page" {
			continue
		}
		made = true
		time.Sleep(100 * time.Millisecond) // auto-attach may deliver it first
		s.mu.Lock()
		known := s.byTarget[ti.TargetID] != nil
		s.mu.Unlock()
		if known {
			continue
		}
		var at struct {
			SessionID string `json:"sessionId"`
		}
		if raw, err := b.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": ti.TargetID, "flatten": true}); err == nil && json.Unmarshal(raw, &at) == nil {
			s.adopt(b, at.SessionID, ti, false)
		}
	}
	if !made {
		if err := call("Target.createTarget", map[string]any{"url": "about:blank"}); err != nil {
			return err
		}
	}
	for t := 0; t < 100; t++ {
		s.mu.Lock()
		n := len(s.tabs)
		s.mu.Unlock()
		if n > 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the browser opened no tab")
}

var deniedPermissions = []string{
	"geolocation", "notifications", "midi", "camera", "microphone", "clipboard-read", "clipboard-write",
	"background-sync", "background-fetch", "periodic-background-sync", "accelerometer", "gyroscope", "magnetometer",
	"ambient-light-sensor", "screen-wake-lock", "display-capture", "idle-detection", "persistent-storage",
	"window-management", "local-fonts", "storage-access", "top-level-storage-access", "payment-handler",
	"speaker-selection", "captured-surface-control", "keyboard-lock", "pointer-lock",
}

// teardown forgets b's state once it is gone; the next use launches a fresh one.
// userAgentLocked is what tabs send: the emulated device's, else Chrome's own. Hold s.mu.
func (s *serverBrowser) userAgentLocked() string {
	if s.dev.ua != "" {
		return s.dev.ua
	}
	return s.ua
}

func (s *serverBrowser) teardown(b cdp) {
	s.mu.Lock()
	if s.br != b {
		s.mu.Unlock()
		return
	}
	s.br, s.tabs, s.active, s.cast = nil, nil, nil, nil
	s.byTarget, s.bySession = map[string]*tab{}, map[string]*tab{}
	s.pressed = inputState{}
	s.mu.Unlock()
	s.pushState()
}

func (s *serverBrowser) idleLoop() {
	for range time.Tick(time.Minute) {
		s.reapSeat()
		s.idleCheck()
	}
}

// idleCheck closes a browser nobody has used or watched for s.idle.
func (s *serverBrowser) idleCheck() {
	s.mu.Lock()
	b := s.br
	stale := b != nil && len(s.viewers) == 0 && s.ctl.who == "" && time.Since(s.lastUse) > s.idle
	s.mu.Unlock()
	if stale {
		b.Close()
	}
}

// Close stops the browser (tests; the server's exit closes the pipe, which ends Chrome too).
func (s *serverBrowser) Close() {
	if b := s.browser(); b != nil {
		b.Close()
	}
}

// ---- CDP helpers ----

// do calls a CDP method on the current browser and decodes the result into out (may be nil).
func (s *serverBrowser) do(ctx context.Context, sid, method string, params, out any) error {
	b := s.browser()
	if b == nil {
		return errBrowserGone
	}
	raw, err := b.Call(ctx, sid, method, params)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// bg is do for fire-and-forget housekeeping.
func (s *serverBrowser) bg(sid, method string, params any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.do(ctx, sid, method, params, nil)
}

type evalResult struct {
	Result struct {
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

func (s *serverBrowser) eval(ctx context.Context, t *tab, expr string, gesture bool) (json.RawMessage, error) {
	var r evalResult
	err := s.do(ctx, t.sid, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true, "userGesture": gesture,
	}, &r)
	if err != nil {
		return nil, err
	}
	if d := r.ExceptionDetails; d != nil {
		msg := d.Exception.Description
		if msg == "" {
			msg = d.Text
		}
		return nil, errors.New(msg)
	}
	return r.Result.Value, nil
}

// pageCall runs one of the page scripts the way the desktop panel does: a throw inside it comes
// back as its own message.
func (s *serverBrowser) pageCall(ctx context.Context, t *tab, code string, gesture bool) (json.RawMessage, error) {
	raw, err := s.eval(ctx, t, "try { ({ ok: "+code+" }) } catch (e) { ({ err: String((e && e.message) || e) }) }", gesture)
	if err != nil {
		return nil, err
	}
	var r struct {
		OK  json.RawMessage `json:"ok"`
		Err string          `json:"err"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Err != "" {
		return nil, errors.New(r.Err)
	}
	return r.OK, nil
}

// ---- CDP events ----

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId"`
}

// onEvent runs on the read loop: it only updates state and starts goroutines.
func (s *serverBrowser) onEvent(b cdp, sid, method string, raw json.RawMessage) {
	switch method {
	case "Target.attachedToTarget":
		var p struct {
			SessionID          string     `json:"sessionId"`
			TargetInfo         targetInfo `json:"targetInfo"`
			WaitingForDebugger bool       `json:"waitingForDebugger"`
		}
		if json.Unmarshal(raw, &p) == nil {
			go s.adopt(b, p.SessionID, p.TargetInfo, p.WaitingForDebugger)
		}
	case "Page.windowOpen":
		var p struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &p) == nil {
			go s.popup(sid, p.URL)
		}
	case "Target.targetDestroyed":
		var p struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(raw, &p) == nil {
			s.removeTab(b, p.TargetID)
		}
	case "Target.targetInfoChanged":
		var p struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(raw, &p) == nil {
			s.infoChanged(p.TargetInfo)
		}
	case "Page.frameStartedLoading", "Page.frameStoppedLoading":
		var p struct {
			FrameID string `json:"frameId"`
		}
		s.mu.Lock()
		if t := s.bySession[sid]; t != nil && json.Unmarshal(raw, &p) == nil && p.FrameID == t.targetID {
			t.loading = method == "Page.frameStartedLoading"
			if t.loading {
				t.err = ""
			}
		}
		s.mu.Unlock()
		s.pushState()
		if method == "Page.frameStoppedLoading" {
			go s.refreshTitles(sid)
		}
	case "Page.screencastFrame":
		s.onFrame(sid, raw)
	case "Page.javascriptDialogOpening":
		go s.dialog(sid, raw)
	case "Runtime.consoleAPICalled":
		var p struct {
			Type string `json:"type"`
			Args []struct {
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"args"`
			StackTrace struct {
				CallFrames []struct {
					URL  string `json:"url"`
					Line int    `json:"lineNumber"`
				} `json:"callFrames"`
			} `json:"stackTrace"`
		}
		if json.Unmarshal(raw, &p) != nil || (p.Type != "warning" && p.Type != "error" && p.Type != "assert") {
			return
		}
		var parts []string
		for _, a := range p.Args {
			var str string
			if json.Unmarshal(a.Value, &str) == nil && a.Value != nil {
				parts = append(parts, str)
			} else if a.Description != "" {
				parts = append(parts, a.Description)
			} else {
				parts = append(parts, string(a.Value))
			}
		}
		level, src := p.Type, ""
		if level == "assert" {
			level = "error"
		}
		if cf := p.StackTrace.CallFrames; len(cf) > 0 && cf[0].URL != "" {
			src = fmt.Sprintf("%s:%d", cf[0].URL, cf[0].Line+1)
		}
		s.addConsole(sid, level, strings.Join(parts, " "), src)
	case "Runtime.exceptionThrown":
		var p struct {
			ExceptionDetails struct {
				Text      string `json:"text"`
				URL       string `json:"url"`
				Line      int    `json:"lineNumber"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(raw, &p) == nil {
			d := p.ExceptionDetails
			msg, _, _ := strings.Cut(d.Exception.Description, "\n    at ")
			if msg == "" {
				msg = d.Text
			}
			src := ""
			if d.URL != "" {
				src = fmt.Sprintf("%s:%d", d.URL, d.Line+1)
			}
			s.addConsole(sid, "error", msg, src)
		}
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Level string `json:"level"`
				Text  string `json:"text"`
				URL   string `json:"url"`
				Line  int    `json:"lineNumber"`
			} `json:"entry"`
		}
		if json.Unmarshal(raw, &p) == nil && (p.Entry.Level == "warning" || p.Entry.Level == "error") {
			src := ""
			if p.Entry.URL != "" {
				src = fmt.Sprintf("%s:%d", p.Entry.URL, p.Entry.Line)
			}
			s.addConsole(sid, p.Entry.Level, p.Entry.Text, src)
		}
	case "Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed":
		s.onNetwork(sid, method, raw)
	case "Fetch.requestPaused":
		go s.paused(sid, raw)
	}
}

func (s *serverBrowser) addConsole(sid, level, msg, src string) {
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300])
	}
	line := "[" + level + "] " + msg
	if src != "" {
		line += " (" + src + ")"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.bySession[sid]; t != nil {
		t.console = append(t.console, line)
		if len(t.console) > maxConsoleLines {
			t.console = t.console[len(t.console)-maxConsoleLines:]
		}
	}
}

// dialog dismisses a JS dialog (a beforeunload one is let through) and tells the agent what it said.
func (s *serverBrowser) dialog(sid string, raw json.RawMessage) {
	var p struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	s.addConsole(sid, "warning", "a "+p.Type+" dialog was dismissed: "+p.Message, "")
	_ = s.bg(sid, "Page.handleJavaScriptDialog", map[string]any{"accept": p.Type == "beforeunload"})
}

// adopt registers a page target. New targets start paused (waiting), so nothing loads before
// Fetch is on. A window the page opened is not listed: its first load is refused in paused and
// the window closed, popup having already sent its URL to the opener's own tab.
func (s *serverBrowser) adopt(b cdp, sid string, info targetInfo, waiting bool) {
	resume := func() {
		if waiting {
			_ = s.bg(sid, "Runtime.runIfWaitingForDebugger", nil)
		}
	}
	s.mu.Lock()
	if s.br != b || s.byTarget[info.TargetID] != nil {
		s.mu.Unlock()
		resume()
		return
	}
	t := &tab{targetID: info.TargetID, sid: sid, url: info.URL, title: tabTitle(info)}
	t.popup = info.OpenerID != "" && s.byTarget[info.OpenerID] != nil
	s.byTarget[t.targetID], s.bySession[sid] = t, t
	vp, ua, mobile := s.vp, s.userAgentLocked(), s.dev.mobile
	s.mu.Unlock()
	if t.popup {
		_ = s.bg(sid, "Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}}})
		resume()
		// ponytail: a window that never loads (window.open("")) is closed after a while.
		time.AfterFunc(10*time.Second, func() { _ = s.bg("", "Target.closeTarget", map[string]any{"targetId": info.TargetID}) })
		return
	}

	for _, c := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil}, {"Runtime.enable", nil}, {"Log.enable", nil},
		{"Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}}}},
		{"Network.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": vp.w, "height": vp.h, "deviceScaleFactor": 1, "mobile": mobile}},
		{"Emulation.setTouchEmulationEnabled", map[string]any{"enabled": mobile}},
		{"Emulation.setUserAgentOverride", map[string]any{"userAgent": ua, "acceptLanguage": "en-US,en"}},
		{"Emulation.setDefaultBackgroundColorOverride", map[string]any{"color": map[string]any{"r": 255, "g": 255, "b": 255, "a": 1}}},
	} {
		_ = s.bg(sid, c.method, c.params)
	}
	s.mu.Lock()
	s.tabs = append(s.tabs, t)
	if s.active == nil {
		s.active = t
	}
	s.mu.Unlock()
	resume()
	s.pushState()
	s.reconcileCast()
}

// popup moves a window.open / target=_blank navigation into the opener's tab, like the desktop
// panel does; it goes through the same Fetch checks as any other load.
// ponytail: a POST form target is replayed as a GET.
func (s *serverBrowser) popup(sid, to string) {
	s.mu.Lock()
	t := s.bySession[sid]
	s.mu.Unlock()
	if t == nil || to == "" || to == "about:blank" {
		return
	}
	s.mu.Lock()
	reason := s.refusal(to)
	s.mu.Unlock()
	if reason != "" {
		s.refused(reason)
		return
	}
	_ = s.bg(sid, "Page.navigate", map[string]any{"url": to})
}

func blockedScheme(to string) string {
	return fmt.Sprintf("blocked: the page tried to go to %s, which the browser doesn't open (only http and https pages), so the browser stayed where it was", to)
}

// refreshTitles reads document titles, which Target.targetInfoChanged reports late or never.
// ponytail: a title JS changes after load reaches viewers only with the next load or tabs call.
func (s *serverBrowser) refreshTitles(sids ...string) {
	for _, sid := range sids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var r evalResult
		err := s.do(ctx, sid, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &r)
		cancel()
		var title string
		if err != nil || json.Unmarshal(r.Result.Value, &title) != nil {
			continue
		}
		s.mu.Lock()
		if t := s.bySession[sid]; t != nil && t.url != "about:blank" {
			t.title = title
		}
		s.mu.Unlock()
	}
	s.pushState()
}

func tabTitle(i targetInfo) string {
	if i.URL == "about:blank" {
		return ""
	}
	return i.Title
}

func (s *serverBrowser) infoChanged(info targetInfo) {
	s.mu.Lock()
	t := s.byTarget[info.TargetID]
	if t != nil && t.url != info.URL {
		t.url, t.title = info.URL, tabTitle(info)
	}
	s.mu.Unlock()
	if t == nil {
		return
	}
	// Whatever got through, a tab never rests on a page that isn't web content.
	if u, err := url.Parse(info.URL); err == nil && !slices.Contains([]string{"http", "https", "about", "chrome-error"}, u.Scheme) {
		go func() { _ = s.bg(t.sid, "Page.navigate", map[string]any{"url": "about:blank"}) }()
	}
	s.pushState()
}

func (s *serverBrowser) removeTab(b cdp, targetID string) {
	s.mu.Lock()
	t := s.byTarget[targetID]
	if t == nil {
		s.mu.Unlock()
		return
	}
	delete(s.byTarget, targetID)
	delete(s.bySession, t.sid)
	i := slices.Index(s.tabs, t)
	if i >= 0 {
		s.tabs = slices.Delete(s.tabs, i, i+1)
	}
	if s.active == t {
		s.active = nil
		if len(s.tabs) > 0 {
			s.active = s.tabs[min(max(i, 0), len(s.tabs)-1)]
		}
	}
	refill := len(s.tabs) == 0 && s.br == b
	s.mu.Unlock()
	if refill {
		go func() { _ = s.bg("", "Target.createTarget", map[string]any{"url": "about:blank"}) }()
	}
	s.pushState()
	go s.reconcileCast()
}

// ---- URL policy (A4) ----

// checkNavURL admits only web pages (and about:blank) as a top-level destination.
func checkNavURL(raw string) error {
	if raw == "about:blank" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %s", raw)
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
		return nil
	}
	return errors.New("the browser only opens http and https pages")
}

func isLocalHost(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || h == "0.0.0.0" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// paused decides a main-frame page load: the scheme policy always, the cross-origin guard while
// an agent action runs. A refused load is aborted, which leaves the page where it was.
func (s *serverBrowser) paused(sid string, raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		FrameID   string `json:"frameId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	s.mu.Lock()
	t := s.bySession[sid]
	var reason string
	if t != nil && p.FrameID == t.targetID {
		reason = s.refusal(p.Request.URL)
	}
	popup := t != nil && t.popup
	s.mu.Unlock()
	if popup { // the opener's tab loads it (popup); this window never does
		_ = s.bg(sid, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "Aborted"})
		_ = s.bg("", "Target.closeTarget", map[string]any{"targetId": t.targetID})
		return
	}
	if reason != "" {
		s.refused(reason)
		_ = s.bg(sid, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "Aborted"})
		return
	}
	_ = s.bg(sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
}

// refusal is why a main-frame load of to is not allowed: the scheme policy always, the
// cross-origin guard while an agent action runs. s.mu is held.
func (s *serverBrowser) refusal(to string) string {
	u, err := url.Parse(to)
	switch {
	case checkNavURL(to) != nil:
		return blockedScheme(to)
	case s.guard != nil && s.guard.on && err == nil && !isLocalHost(u.Hostname()) && !s.guard.hosts[strings.ToLower(u.Hostname())]:
		return fmt.Sprintf("blocked: the page tried to go to %s, a site you haven't been cleared to visit, so the browser stayed where it was. To go there, call navigate with that URL; the user is asked first", to)
	}
	return ""
}

// refused reports a stopped navigation: to the agent's action if one runs, else to the person
// who navigated last.
func (s *serverBrowser) refused(reason string) {
	s.mu.Lock()
	if s.guard != nil {
		s.guard.blocked = append(s.guard.blocked, reason)
		s.mu.Unlock()
		return
	}
	peer, fresh := s.navBy, time.Since(s.navAt) < 30*time.Second
	s.mu.Unlock()
	if fresh {
		s.notify(peer, strings.TrimPrefix(reason, "blocked: "))
	}
}

// notify tells one peer something went wrong, as an error event.
func (s *serverBrowser) notify(peerID, msg string) {
	if p, ok := s.peers.Get(peerID); ok {
		_ = p.ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: msg})
	}
}

// ---- agent actions ----

type browseArgs struct {
	Action     string   `json:"action"`
	URL        string   `json:"url"`
	Ref        string   `json:"ref"`
	Text       string   `json:"text"`
	Submit     bool     `json:"submit"`
	Key        string   `json:"key"`
	Direction  string   `json:"direction"`
	Expression string   `json:"expression"`
	Ms         int      `json:"ms"`
	Op         string   `json:"op"`
	Tab        int      `json:"tab"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	To         string   `json:"to"`
	Button     string   `json:"button"`
	Double     bool     `json:"double"`
	FullPage   bool     `json:"full_page"`
	Files      []string `json:"files"`
	Device     string   `json:"device"`
	Filter     string   `json:"filter"`
}

// Browse runs one agent action in the server's browser. The agent holds the controller seat for
// its duration unless a human has it, in which case it fails at once; a human taking over mid-way
// ends the action with the same message.
func (s *serverBrowser) Browse(ctx context.Context, sessionID, action string, args json.RawMessage, g browserGuard) (tool.BrowserResult, error) {
	var a browseArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.BrowserResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if sessionID == "" {
		return tool.BrowserResult{}, errors.New("the browser tool needs a chat to act for")
	}
	if err := s.ensure(ctx); err != nil {
		return tool.BrowserResult{}, err
	}
	select {
	case s.act <- struct{}{}:
	case <-ctx.Done():
		return tool.BrowserResult{}, ctx.Err()
	}
	defer func() { <-s.act }()

	actx, epoch, end, err := s.beginAgent(ctx, g)
	if err != nil {
		return tool.BrowserResult{}, err
	}
	res, err := s.runAction(actx, sessionID, action, a)
	blocked, taken := end(epoch)
	switch {
	case taken:
		return tool.BrowserResult{}, errHumanHasIt
	case blocked != "":
		return tool.BrowserResult{}, errors.New(blocked)
	}
	return res, err
}

func (s *serverBrowser) runAction(ctx context.Context, sessionID, action string, a browseArgs) (tool.BrowserResult, error) {
	t, err := s.tabFor(ctx, sessionID)
	if err != nil {
		return tool.BrowserResult{}, err
	}
	note := ""
	switch action {
	case "navigate":
		if err := checkNavURL(a.URL); err != nil {
			return tool.BrowserResult{}, err
		}
		s.clearNet(t)
		s.setErr(t, "", true)
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		if err := s.do(ctx, t.sid, "Page.navigate", map[string]any{"url": a.URL}, &nav); err != nil {
			return tool.BrowserResult{}, err
		}
		// A superseded load (a redirect, a client-side router) is aborted, not failed.
		if nav.ErrorText != "" && !strings.Contains(nav.ErrorText, "ERR_ABORTED") {
			s.setErr(t, strings.TrimPrefix(nav.ErrorText, "net::")+" ("+a.URL+")", false)
		}
	case "snapshot":
	case "click":
		at, err := s.locate(ctx, t, a.Ref)
		if err != nil {
			return tool.BrowserResult{}, err
		}
		// A real click where the element is, a synthetic one if it can't be aimed at.
		plain := (a.Button == "" || a.Button == "left") && !a.Double
		if at.Visible && at.W > 0 && at.X >= 0 && at.Y >= 0 && at.X < at.W && at.Y < at.H {
			for _, c := range clickCmdsWith(at.X, at.Y, a.Button, a.Double) {
				if err := s.do(ctx, t.sid, c.Method, c.Params, nil); err != nil {
					return tool.BrowserResult{}, err
				}
			}
		} else if !plain {
			return tool.BrowserResult{}, errors.New("that element can't be reached with the pointer; scroll it into view")
		} else if _, err := s.pageCall(ctx, t, js("clickJs", "__REF__", jsStr(a.Ref)), true); err != nil {
			return tool.BrowserResult{}, err
		}
	case "type":
		if _, err := s.pageCall(ctx, t, js("focusJs", "__REF__", jsStr(a.Ref)), true); err != nil {
			return tool.BrowserResult{}, err
		}
		if err := s.do(ctx, t.sid, "Input.insertText", map[string]any{"text": a.Text}, nil); err != nil {
			return tool.BrowserResult{}, err
		}
		if a.Submit {
			if err := s.press(ctx, t, "Enter"); err != nil {
				return tool.BrowserResult{}, err
			}
		}
	case "press":
		if err := s.press(ctx, t, a.Key); err != nil {
			return tool.BrowserResult{}, err
		}
	case "scroll":
		code := js("scrollPage", "__DIR__", map[bool]string{true: "-1", false: "1"}[a.Direction == "up"])
		if a.Ref != "" {
			code = js("scrollRef", "__REF__", jsStr(a.Ref))
		}
		if _, err := s.pageCall(ctx, t, code, false); err != nil {
			return tool.BrowserResult{}, err
		}
	case "evaluate":
		v, err := s.eval(ctx, t, a.Expression, true)
		if err != nil {
			return tool.BrowserResult{}, err
		}
		if len(v) == 0 {
			v = json.RawMessage("null")
		}
		var buf bytes.Buffer
		if json.Indent(&buf, v, "", " ") != nil {
			buf.Reset()
			buf.Write(v)
		}
		out := buf.String()
		if r := []rune(out); len(r) > 20_000 {
			out = string(r[:20_000]) + "\n[truncated]"
		}
		return tool.BrowserResult{Output: out}, nil
	case "back", "forward":
		s.clearNet(t)
		if err := s.history(ctx, t, action); err != nil {
			return tool.BrowserResult{}, err
		}
	case "reload":
		s.clearNet(t)
		if err := s.do(ctx, t.sid, "Page.reload", nil, nil); err != nil {
			return tool.BrowserResult{}, err
		}
	case "hover":
		at, err := s.locate(ctx, t, a.Ref)
		if err != nil {
			return tool.BrowserResult{}, err
		}
		if !at.Visible || at.X < 0 || at.Y < 0 || at.X >= at.W || at.Y >= at.H {
			return tool.BrowserResult{}, errors.New("that element can't be reached with the pointer; scroll it into view")
		}
		if err := s.do(ctx, t.sid, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": at.X, "y": at.Y}, nil); err != nil {
			return tool.BrowserResult{}, err
		}
		sleep(ctx, 150*time.Millisecond) // a hover menu opens on the next frames
	case "select":
		if _, err := s.pageCall(ctx, t, js("selectJs", "__REF__", jsStr(a.Ref), "__WANT__", jsStr(a.Text)), true); err != nil {
			return tool.BrowserResult{}, err
		}
	case "wait":
		ms := min(max(a.Ms, 0), 30_000)
		if a.Text == "" {
			sleep(ctx, time.Duration(ms)*time.Millisecond)
			break
		}
		limit := ms
		if limit == 0 {
			limit = 10_000
		}
		found := false
		for waited := 0; !found && waited < limit; waited += 200 {
			raw, err := s.eval(ctx, t, js("hasText", "__TEXT__", jsStr(a.Text)), false)
			if err != nil {
				return tool.BrowserResult{}, err
			}
			if found = string(raw) == "true"; !found {
				sleep(ctx, 200*time.Millisecond)
			}
		}
		if !found {
			snap, _ := s.snapshot(ctx, t)
			return tool.BrowserResult{}, fmt.Errorf("Timed out after %d ms waiting for %s. The page as it is:\n\n%s", limit, jsStr(a.Text), snap)
		}
	case "tabs":
		switch a.Op {
		case "list":
			s.refreshTitles(s.tabSessions(sessionID)...)
			return tool.BrowserResult{Output: s.tabList(sessionID)}, nil
		case "new":
			if _, err := s.newTab(ctx, sessionID); err != nil {
				return tool.BrowserResult{}, err
			}
			return tool.BrowserResult{Output: s.tabList(sessionID)}, nil
		case "select", "close":
			target, err := s.tabAt(a.Tab-1, sessionID)
			if err != nil {
				return tool.BrowserResult{}, fmt.Errorf("there is no tab %d; %s", a.Tab, s.tabList(sessionID))
			}
			if a.Op == "select" {
				s.selectTab(target)
			} else if err := s.closeTab(ctx, target); err != nil {
				return tool.BrowserResult{}, err
			}
			if t, err = s.tabFor(ctx, sessionID); err != nil {
				return tool.BrowserResult{}, err
			}
			note = s.tabList(sessionID) + "\n\n"
		default:
			return tool.BrowserResult{}, errors.New("tabs needs op list, new, select or close")
		}
	case "resize":
		if a.Width < minVPW || a.Width > maxVPDim || a.Height < minVPH || a.Height > maxVPDim {
			return tool.BrowserResult{}, fmt.Errorf("resize takes width %d-%d and height %d-%d", minVPW, maxVPDim, minVPH, maxVPDim)
		}
		s.setViewport(a.Width, a.Height)
		sleep(ctx, 200*time.Millisecond) // the page re-lays out before it is read
	case "emulate":
		d := devices[a.Device]
		s.mu.Lock()
		s.dev, s.vp = device{mobile: d.mobile, ua: d.ua}, viewport{} // zero, so setViewport applies it even at the same size
		s.mu.Unlock()
		s.setViewport(d.w, d.h)
		for _, tb := range s.allTabs() {
			_ = s.bg(tb.sid, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": d.mobile})
			s.mu.Lock()
			ua := s.userAgentLocked()
			s.mu.Unlock()
			_ = s.bg(tb.sid, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ua, "acceptLanguage": "en-US,en"})
		}
		sleep(ctx, 200*time.Millisecond)
		if err := s.do(ctx, t.sid, "Page.reload", nil, nil); err != nil { // the user agent applies to the next load
			return tool.BrowserResult{}, err
		}
	case "network":
		return tool.BrowserResult{Output: s.networkList(t, a.Filter)}, nil
	case "drag":
		from, err := s.locate(ctx, t, a.Ref)
		if err != nil {
			return tool.BrowserResult{}, err
		}
		if err := s.dragTo(ctx, t, from, a.To); err != nil {
			return tool.BrowserResult{}, err
		}
	case "upload":
		if err := s.upload(ctx, t, a.Ref, a.Files); err != nil {
			return tool.BrowserResult{}, err
		}
	case "screenshot":
		return s.screenshot(ctx, t, a)
	default:
		return tool.BrowserResult{}, fmt.Errorf("unknown browser action %s", action)
	}

	s.settle(ctx, t)
	if slices.Contains([]string{"navigate", "back", "forward", "reload"}, action) {
		if msg := s.loadError(ctx, t); msg != "" {
			return tool.BrowserResult{}, errors.New("couldn't load the page: " + msg)
		}
	}
	snap, err := s.snapshot(ctx, t)
	return tool.BrowserResult{Output: note + snap}, err
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// settle lets a navigation an action set off start, then waits (bounded) for it to finish.
func (s *serverBrowser) settle(ctx context.Context, t *tab) {
	sleep(ctx, 250*time.Millisecond)
	for waited := 0; waited < 15_000 && ctx.Err() == nil; waited += 100 {
		s.mu.Lock()
		loading := t.loading
		s.mu.Unlock()
		if !loading {
			return
		}
		sleep(ctx, 100*time.Millisecond)
	}
}

func (s *serverBrowser) setErr(t *tab, msg string, force bool) {
	s.mu.Lock()
	if force || t.err == "" {
		t.err = msg
	}
	s.mu.Unlock()
}

// loadError is why the tab's last load failed: what navigate reported, else Chrome's own error page.
func (s *serverBrowser) loadError(ctx context.Context, t *tab) string {
	s.mu.Lock()
	msg, cur := t.err, t.url
	s.mu.Unlock()
	if msg == "" {
		raw, _ := s.eval(ctx, t, `location.protocol === "chrome-error:" ? ((document.querySelector(".error-code") || {}).textContent || document.title || "the page didn't load") : ""`, false)
		var code string
		_ = json.Unmarshal(raw, &code)
		if code != "" {
			msg = code + " (" + cur + ")"
		}
	}
	return msg
}

func (s *serverBrowser) snapshot(ctx context.Context, t *tab) (string, error) {
	raw, err := s.eval(ctx, t, jsSections["snapshot"], false)
	if err != nil {
		return "", err
	}
	var text string
	_ = json.Unmarshal(raw, &text)
	s.mu.Lock()
	log := t.console
	t.console = nil
	s.mu.Unlock()
	if len(log) > 0 {
		text += "\n\nConsole since the last snapshot:\n" + strings.Join(log, "\n")
	}
	return text, nil
}

type located struct {
	X, Y, W, H int
	Visible    bool
}

func (s *serverBrowser) locate(ctx context.Context, t *tab, ref string) (located, error) {
	raw, err := s.pageCall(ctx, t, js("locateJs", "__REF__", jsStr(ref), "__REFPLAIN__", plainRef.ReplaceAllString(ref, "")), false)
	var at located
	if err == nil {
		var r struct {
			X, Y, W, H int
			Visible    bool `json:"visible"`
		}
		err = json.Unmarshal(raw, &r)
		at = located{r.X, r.Y, r.W, r.H, r.Visible}
	}
	return at, err
}

func (s *serverBrowser) press(ctx context.Context, t *tab, combo string) error {
	cmds, err := pressCmds(combo)
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if err := s.do(ctx, t.sid, c.Method, c.Params, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *serverBrowser) history(ctx context.Context, t *tab, dir string) error {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := s.do(ctx, t.sid, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	i := h.CurrentIndex + map[string]int{"back": -1, "forward": 1}[dir]
	if i < 0 || i >= len(h.Entries) {
		return fmt.Errorf("nothing to go %s to", dir)
	}
	return s.do(ctx, t.sid, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
}

// screenshot is the viewport (or one element, or the whole page) as a JPEG no wider than
// browserShotWidth.
func (s *serverBrowser) screenshot(ctx context.Context, t *tab, a browseArgs) (tool.BrowserResult, error) {
	s.mu.Lock()
	vp := s.vp
	pageURL, title := t.url, t.title
	s.mu.Unlock()
	x, y, w, h := 0, 0, vp.w, vp.h
	what := ""
	params := map[string]any{"format": "jpeg", "quality": 80}
	switch {
	case a.Ref != "":
		raw, err := s.pageCall(ctx, t, js("rectJs", "__REF__", jsStr(a.Ref), "__REFPLAIN__", plainRef.ReplaceAllString(a.Ref, "")), false)
		if err != nil {
			return tool.BrowserResult{}, err
		}
		var r struct{ X, Y, W, H float64 }
		if err := json.Unmarshal(raw, &r); err != nil || r.W < 1 || r.H < 1 {
			return tool.BrowserResult{}, errors.New("that element has no size to capture")
		}
		x, y, w, h = int(r.X), int(r.Y), min(int(r.W+0.5), 4*browserShotWidth), min(int(r.H+0.5), maxShotHeight)
		what, params["captureBeyondViewport"] = " (element)", true
	case a.FullPage:
		var m struct {
			Size struct{ Width, Height float64 } `json:"cssContentSize"`
		}
		if err := s.do(ctx, t.sid, "Page.getLayoutMetrics", nil, &m); err != nil {
			return tool.BrowserResult{}, fmt.Errorf("the screenshot failed: %w", err)
		}
		w, h = max(vp.w, int(m.Size.Width)), min(max(vp.h, int(m.Size.Height)), maxShotHeight)
		what, params["captureBeyondViewport"] = " (full page)", true
		params["quality"] = 70
	}
	scale := min(1, float64(browserShotWidth)/float64(w))
	if scale < 1 || what != "" {
		params["clip"] = map[string]any{"x": x, "y": y, "width": w, "height": h, "scale": scale}
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := s.do(ctx, t.sid, "Page.captureScreenshot", params, &shot); err != nil {
		return tool.BrowserResult{}, fmt.Errorf("the screenshot failed: %w", err)
	}
	iw, ih := jpegSize(shot.Data, int(float64(w)*scale+0.5), int(float64(h)*scale+0.5))
	return tool.BrowserResult{
		Output: fmt.Sprintf("Screenshot%s of %s (%s)", what, pageURL, title),
		Image:  &protocol.ImageData{MimeType: "image/jpeg", Data: shot.Data, Width: iw, Height: ih},
	}, nil
}

// ---- tabs ----

func (s *serverBrowser) activeTab() *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// tabAt is the i'th tab (from 0) of chat owner's own, or of all of them when owner is anyOwner
// (a person at the panel sees every tab).
func (s *serverBrowser) tabAt(i int, owner string) (*tab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	own := s.ownedLocked(owner)
	if i < 0 || i >= len(own) {
		return nil, errors.New("no such tab")
	}
	return own[i], nil
}

// anyOwner is the owner argument that matches every tab; no chat id contains a NUL.
const anyOwner = "\x00"

func (s *serverBrowser) ownedLocked(owner string) []*tab {
	if owner == anyOwner {
		return s.tabs
	}
	var out []*tab
	for _, t := range s.tabs {
		if t.owner == owner {
			out = append(out, t)
		}
	}
	return out
}

func (s *serverBrowser) tabSessions(owner string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, t := range s.ownedLocked(owner) {
		out = append(out, t.sid)
	}
	return out
}

// tabList is chat owner's tabs as the agent numbers them (from 1).
func (s *serverBrowser) tabList(owner string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("Tabs:")
	for i, t := range s.ownedLocked(owner) {
		title := t.title
		if title == "" {
			title = "New tab"
		}
		sel := ""
		if t == s.active {
			sel = " (selected)"
		}
		fmt.Fprintf(&b, "\n%d.%s %s — %s", i+1, sel, title, t.url)
	}
	return b.String()
}

// tabFor is the tab chat sid's agent acts on, made the active one so viewers see it: one it owns
// (the active one if it owns that), else a new one. A tab's owner is fixed when it is created,
// except that a pristine unowned tab (one blank history entry, nothing visited) is claimed, so the
// browser's first tab isn't wasted. It never returns another chat's tab.
func (s *serverBrowser) tabFor(ctx context.Context, sid string) (*tab, error) {
	s.mu.Lock()
	var own, blank *tab
	for _, t := range s.tabs {
		switch {
		case t.owner == sid && (own == nil || t == s.active):
			own = t
		case t.owner == "" && blank == nil && t.url == "about:blank":
			blank = t
		}
	}
	active := s.active
	s.mu.Unlock()
	if own == nil && blank != nil && s.pristine(ctx, blank) {
		s.mu.Lock()
		if blank.owner == "" {
			blank.owner, own = sid, blank
		}
		s.mu.Unlock()
	}
	if own == nil {
		return s.newTab(ctx, sid)
	}
	if own != active {
		s.selectTab(own)
	}
	return own, nil
}

// pristine reports whether t has never been anywhere: a history of exactly one about:blank.
func (s *serverBrowser) pristine(ctx context.Context, t *tab) bool {
	var h struct {
		Entries []struct {
			URL string `json:"url"`
		} `json:"entries"`
	}
	return s.do(ctx, t.sid, "Page.getNavigationHistory", nil, &h) == nil && len(h.Entries) == 1 && h.Entries[0].URL == "about:blank"
}

func (s *serverBrowser) newTab(ctx context.Context, owner string) (*tab, error) {
	s.mu.Lock()
	n, mine := len(s.tabs), len(s.ownedLocked(owner))
	s.mu.Unlock()
	if n >= maxTabs || mine >= maxOwnerTabs {
		return nil, fmt.Errorf("too many tabs open (%d); close one first", mine)
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := s.do(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return nil, err
	}
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		t := s.byTarget[created.TargetID]
		inList := t != nil && slices.Contains(s.tabs, t)
		if inList {
			t.owner = owner
		}
		s.mu.Unlock()
		if inList {
			s.selectTab(t)
			return t, nil
		}
		sleep(ctx, 50*time.Millisecond)
	}
	return nil, errors.New("the new tab didn't open")
}

func (s *serverBrowser) selectTab(t *tab) {
	s.mu.Lock()
	s.active = t
	s.mu.Unlock()
	go func() { _ = s.bg("", "Target.activateTarget", map[string]any{"targetId": t.targetID}) }()
	s.pushState()
	go s.reconcileCast()
}

func (s *serverBrowser) closeTab(ctx context.Context, t *tab) error {
	if err := s.do(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.targetID}, nil); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		_, gone := s.byTarget[t.targetID]
		n := len(s.tabs)
		s.mu.Unlock()
		if !gone && n > 0 {
			return nil
		}
		sleep(ctx, 50*time.Millisecond)
	}
	return errors.New("the tab didn't close")
}
