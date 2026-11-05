package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// fakeCDP records the calls a serverBrowser makes instead of talking to Chrome.
type fakeCDP struct {
	mu    sync.Mutex
	calls []string // "sid method {params json}"
	done  chan struct{}
	once  sync.Once
	// visited is how many pages a tab's history holds (0 or 1: pristine, as a new tab is).
	visited int
}

func newFakeCDP() *fakeCDP { return &fakeCDP{done: make(chan struct{})} }

func (f *fakeCDP) Call(_ context.Context, sid, method string, params any) (json.RawMessage, error) {
	p, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s", sid, method, p))
	visited := f.visited
	f.mu.Unlock()
	if method == "Page.getNavigationHistory" {
		entries := `[{"id":1,"url":"about:blank"}`
		for i := 0; i < visited; i++ {
			entries += fmt.Sprintf(`,{"id":%d,"url":"https://person.example/%d"}`, i+2, i)
		}
		return json.RawMessage(`{"currentIndex":0,"entries":` + entries + `]}`), nil
	}
	return json.RawMessage(`{}`), nil
}
func (f *fakeCDP) Subscribe(func(string, string, json.RawMessage)) func() { return func() {} }
func (f *fakeCDP) Done() <-chan struct{}                                  { return f.done }
func (f *fakeCDP) Close()                                                 { f.once.Do(func() { close(f.done) }) }

func (f *fakeCDP) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, " "+method+" ") {
			n++
		}
	}
	return n
}

func (f *fakeCDP) has(method, params string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, " "+method+" ") && strings.Contains(c, params) })
}

// fakeBrowser is a serverBrowser over a fakeCDP with one tab, S1.
func fakeBrowser(t *testing.T, peers *PeerRegistry) (*serverBrowser, *fakeCDP) {
	t.Helper()
	s := newServerBrowser(peers, t.TempDir(), func() []uint16 { return nil })
	f := newFakeCDP()
	tb := &tab{targetID: "T1", sid: "S1", url: "about:blank"}
	s.br, s.tabs, s.active = f, []*tab{tb}, tb
	s.byTarget["T1"], s.bySession["S1"] = tb, tb
	s.launched = nil
	return s, f
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckNavURL(t *testing.T) {
	for _, ok := range []string{"http://example.com/", "https://EXAMPLE.com:8443/a?b#c", "http://localhost:3000", "http://[::1]:80/", "about:blank"} {
		if err := checkNavURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "file:///etc/passwd", "FILE:///etc/passwd", "chrome://settings", "chrome-error://chromewebdata/", "devtools://devtools/bundled/inspector.html",
		"javascript:alert(1)", "view-source:http://example.com", "data:text/html,<p>x", "blob:http://example.com/1", "ftp://example.com/",
		"http://", "http:///path", "//example.com/", "about:srcdoc", "about:blank#x", "http://exa mple.com/",
	} {
		if checkNavURL(bad) == nil {
			t.Errorf("%q was allowed", bad)
		}
	}
}

func TestIsLocalHost(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1", "[::1]", "app.localhost", "0.0.0.0"} {
		if !isLocalHost(h) {
			t.Errorf("%s not local", h)
		}
	}
	for _, h := range []string{"example.com", "127.0.0.1.evil.com", "localhost.evil.com", "10.0.0.1", "192.168.1.1", ""} {
		if isLocalHost(h) {
			t.Errorf("%s is local", h)
		}
	}
}

// The request filter: scheme policy always, the cross-origin guard only while an action runs,
// and only for main-frame loads.
func TestPausedPolicy(t *testing.T) {
	s, f := fakeBrowser(t, NewPeerRegistry())
	paused := func(frame, to string) {
		raw, _ := json.Marshal(map[string]any{"requestId": "r", "frameId": frame, "request": map[string]any{"url": to}})
		s.paused("S1", raw)
	}
	verdict := func(frame, to string) string {
		f.mu.Lock()
		f.calls = nil
		f.mu.Unlock()
		paused(frame, to)
		switch {
		case f.count("Fetch.failRequest") == 1 && f.has("Fetch.failRequest", `"Aborted"`):
			return "abort"
		case f.count("Fetch.continueRequest") == 1:
			return "continue"
		}
		return "?"
	}

	if v := verdict("T1", "https://example.com/"); v != "continue" {
		t.Errorf("web page without a guard: %s", v)
	}
	for _, bad := range []string{"file:///etc/passwd", "chrome://version", "data:text/html,x", "view-source:http://a.test/", "blob:http://a.test/1"} {
		if v := verdict("T1", bad); v != "abort" {
			t.Errorf("%s: %s", bad, v)
		}
	}
	if v := verdict("SUBFRAME", "file:///etc/passwd"); v != "continue" {
		t.Errorf("a subframe load is not a navigation: %s", v)
	}

	s.guard = &navGuard{on: true, hosts: map[string]bool{"approved.test": true}}
	for to, want := range map[string]string{
		"http://approved.test/x":          "continue",
		"http://APPROVED.test/x":          "continue",
		"http://localhost:5173/":          "continue",
		"http://127.0.0.1:3000/":          "continue",
		"http://other.test/":              "abort",
		"http://approved.test.evil.test/": "abort",
		"file:///etc/hostname":            "abort",
	} {
		if v := verdict("T1", to); v != want {
			t.Errorf("guarded %s: %s, want %s", to, v, want)
		}
	}
	if n := len(s.guard.blocked); n != 3 {
		t.Fatalf("blocked = %v", s.guard.blocked)
	}
	if b := strings.Join(s.guard.blocked, "\n"); !strings.Contains(b, "the page tried to go to http://other.test/, a site you haven't been cleared to visit, so the browser stayed where it was. To go there, call navigate with that URL; the user is asked first") {
		t.Errorf("blocked message: %s", b)
	}
	s.guard.on = false // browserBlockCrossOrigin off: only the scheme policy remains
	if v := verdict("T1", "http://other.test/"); v != "continue" {
		t.Errorf("guard off: %s", v)
	}
	if v := verdict("T1", "file:///x"); v != "abort" {
		t.Errorf("guard off, file: %s", v)
	}
}

func TestInputValidation(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	st := &inputState{vpW: 800, vpH: 600}
	bad := map[string]protocol.BrowserInputData{
		"no kind":          {},
		"unknown kind":     {Kind: "touch"},
		"mouse no coords":  {Kind: "mouse", Type: "move"},
		"mouse nan":        {Kind: "mouse", Type: "move", X: f(math.NaN()), Y: f(1)},
		"mouse inf":        {Kind: "mouse", Type: "move", X: f(1), Y: f(math.Inf(1))},
		"mouse bad type":   {Kind: "mouse", Type: "drag", X: f(1), Y: f(1)},
		"mouse bad button": {Kind: "mouse", Type: "down", Button: "back", X: f(1), Y: f(1)},
		"mouse no button":  {Kind: "mouse", Type: "down", X: f(1), Y: f(1)},
		"mouse clicks":     {Kind: "mouse", Type: "down", Button: "left", Clicks: 9, X: f(1), Y: f(1)},
		"neg clicks":       {Kind: "mouse", Type: "down", Button: "left", Clicks: -1, X: f(1), Y: f(1)},
		"wheel no coords":  {Kind: "wheel", DY: 1},
		"wheel inf":        {Kind: "wheel", X: f(1), Y: f(1), DY: math.Inf(-1)},
		"key no type":      {Kind: "key", Key: "a"},
		"key move":         {Kind: "key", Type: "move", Key: "a"},
		"key empty":        {Kind: "key", Type: "down"},
		"key long":         {Kind: "key", Type: "down", Key: strings.Repeat("a", 33)},
		"code long":        {Kind: "key", Type: "down", Key: "a", Code: strings.Repeat("a", 33)},
		"key bad utf8":     {Kind: "key", Type: "down", Key: "\xff"},
		"mods high":        {Kind: "key", Type: "down", Key: "a", Modifiers: 16},
		"mods negative":    {Kind: "key", Type: "down", Key: "a", Modifiers: -1},
		"text empty":       {Kind: "text"},
		"text too long":    {Kind: "text", Text: strings.Repeat("x", maxInputText+1)},
		"text bad utf8":    {Kind: "text", Text: "a\xffb"},
	}
	for name, in := range bad {
		if cmds, err := st.apply(in); err == nil {
			t.Errorf("%s accepted: %v", name, cmds)
		}
	}
	if st.buttons != 0 || len(st.keys) != 0 {
		t.Errorf("rejected input left state behind: %+v", st)
	}

	// Coordinates are clamped to the viewport.
	cmds, err := st.apply(protocol.BrowserInputData{Kind: "mouse", Type: "move", X: f(-50), Y: f(99999)})
	if err != nil || cmds[0].Params["x"] != 0.0 || cmds[0].Params["y"] != 599.0 {
		t.Errorf("clamp: %v, %v", cmds, err)
	}
	cmds, err = st.apply(protocol.BrowserInputData{Kind: "wheel", X: f(10), Y: f(10), DX: 1e9, DY: -1e9})
	if err != nil || cmds[0].Params["deltaX"] != float64(maxWheel) || cmds[0].Params["deltaY"] != float64(-maxWheel) {
		t.Errorf("wheel clamp: %v, %v", cmds, err)
	}
	if _, err := st.apply(protocol.BrowserInputData{Kind: "text", Text: strings.Repeat("é", maxInputText/2)}); err != nil {
		t.Errorf("a 4 KiB text: %v", err)
	}
}

func TestInputMapping(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	st := &inputState{vpW: 800, vpH: 600}
	apply := func(in protocol.BrowserInputData) cdpCmd {
		t.Helper()
		cmds, err := st.apply(in)
		if err != nil || len(cmds) != 1 {
			t.Fatalf("%+v: %v, %v", in, cmds, err)
		}
		return cmds[0]
	}

	down := apply(protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "right", Clicks: 2, X: f(10), Y: f(20), Modifiers: 8})
	if down.Method != "Input.dispatchMouseEvent" || down.Params["type"] != "mousePressed" || down.Params["buttons"] != 2 || down.Params["clickCount"] != 2 || down.Params["modifiers"] != 8 {
		t.Errorf("down: %v", down)
	}
	mv := apply(protocol.BrowserInputData{Kind: "mouse", Type: "move", X: f(11), Y: f(21)})
	if mv.Params["type"] != "mouseMoved" || mv.Params["button"] != "right" || mv.Params["buttons"] != 2 {
		t.Errorf("a drag keeps its button: %v", mv)
	}
	up := apply(protocol.BrowserInputData{Kind: "mouse", Type: "up", Button: "right", X: f(11), Y: f(21)})
	if up.Params["type"] != "mouseReleased" || up.Params["buttons"] != 0 || up.Params["clickCount"] != 1 {
		t.Errorf("up: %v", up)
	}
	if w := apply(protocol.BrowserInputData{Kind: "wheel", X: f(1), Y: f(2), DY: 120}); w.Params["type"] != "mouseWheel" || w.Params["deltaY"] != 120.0 {
		t.Errorf("wheel: %v", w)
	}

	// Printable keys are text; the client sends no key event for them.
	if txt := apply(protocol.BrowserInputData{Kind: "text", Text: "héllo"}); txt.Method != "Input.insertText" || txt.Params["text"] != "héllo" {
		t.Errorf("text: %v", txt)
	}
	// Enter types a newline; Backspace is a raw key; a Ctrl+letter is a raw key plus its editing command.
	if k := apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "Enter", Code: "Enter"}); k.Params["type"] != "keyDown" || k.Params["text"] != "\r" || k.Params["windowsVirtualKeyCode"] != 13 {
		t.Errorf("enter: %v", k)
	}
	if k := apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "Backspace", Code: "Backspace"}); k.Params["type"] != "rawKeyDown" || k.Params["text"] != nil || k.Params["windowsVirtualKeyCode"] != 8 {
		t.Errorf("backspace: %v", k)
	}
	for _, tc := range []struct {
		key  string
		mods int
		cmd  string
	}{{"a", modCtrl, "selectAll"}, {"c", modMeta, "copy"}, {"x", modCtrl, "cut"}, {"v", modMeta, "paste"}, {"z", modMeta, "undo"}, {"z", modMeta | modShift, "redo"}, {"y", modCtrl, "redo"}} {
		k := apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: tc.key, Code: "Key" + strings.ToUpper(tc.key), Modifiers: tc.mods})
		if k.Params["type"] != "rawKeyDown" || k.Params["text"] != nil || !slices.Equal(k.Params["commands"].([]string), []string{tc.cmd}) {
			t.Errorf("%s+%d: %v", tc.key, tc.mods, k)
		}
	}
	if k := apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "a", Code: "KeyA", Modifiers: modAlt}); k.Params["commands"] != nil || k.Params["text"] != nil {
		t.Errorf("alt+a: %v", k)
	}
	if k := apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "ArrowLeft", Code: "ArrowLeft", Modifiers: modShift}); k.Params["windowsVirtualKeyCode"] != 37 || k.Params["modifiers"] != modShift {
		t.Errorf("shift+left: %v", k)
	}
}

// What the controller still holds when it lets go (or drops) is released.
func TestInputReleaseAll(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	st := &inputState{vpW: 800, vpH: 600}
	_, _ = st.apply(protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "left", X: f(30), Y: f(40)})
	_, _ = st.apply(protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "middle", X: f(30), Y: f(40)})
	_, _ = st.apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "Shift", Code: "ShiftLeft", Modifiers: 8})
	_, _ = st.apply(protocol.BrowserInputData{Kind: "key", Type: "down", Key: "ArrowDown", Code: "ArrowDown"})
	_, _ = st.apply(protocol.BrowserInputData{Kind: "key", Type: "up", Key: "ArrowDown", Code: "ArrowDown"})
	rel := st.releaseAll()
	var buttons, keys []string
	for _, c := range rel {
		if c.Params["type"] == "mouseReleased" {
			buttons = append(buttons, c.Params["button"].(string))
		} else if c.Params["type"] == "keyUp" {
			keys = append(keys, c.Params["key"].(string))
		}
	}
	slices.Sort(buttons)
	if !slices.Equal(buttons, []string{"left", "middle"}) || !slices.Equal(keys, []string{"Shift"}) {
		t.Errorf("released buttons %v keys %v", buttons, keys)
	}
	if again := st.releaseAll(); len(again) != 0 || st.buttons != 0 {
		t.Errorf("second release: %v", again)
	}
}

func TestPressCmds(t *testing.T) {
	cmds, err := pressCmds("Control+a")
	if err != nil || len(cmds) != 2 || cmds[0].Params["type"] != "rawKeyDown" || cmds[0].Params["modifiers"] != modCtrl || cmds[0].Params["commands"] == nil || cmds[1].Params["type"] != "keyUp" {
		t.Errorf("Control+a: %v, %v", cmds, err)
	}
	if cmds, _ := pressCmds("x"); cmds[0].Params["text"] != "x" || cmds[0].Params["type"] != "keyDown" {
		t.Errorf("x: %v", cmds)
	}
	if cmds, _ := pressCmds("Up"); cmds[0].Params["key"] != "ArrowUp" || cmds[0].Params["windowsVirtualKeyCode"] != 38 {
		t.Errorf("Up: %v", cmds)
	}
	if cmds, _ := pressCmds("enter"); cmds[0].Params["key"] != "Enter" || cmds[0].Params["text"] != "\r" {
		t.Errorf("enter: %v", cmds)
	}
	if cmds, _ := pressCmds("Shift+Tab"); cmds[0].Params["key"] != "Tab" || cmds[0].Params["modifiers"] != modShift {
		t.Errorf("Shift+Tab: %v", cmds)
	}
	if cmds, _ := pressCmds("Ctrl++"); cmds[0].Params["key"] != "+" {
		t.Errorf("Ctrl++: %v", cmds)
	}
	if _, err := pressCmds("Frobnicate"); err == nil {
		t.Error("an unknown key was accepted")
	}
}

func TestJSSections(t *testing.T) {
	for _, name := range []string{"snapshot", "locateJs", "focusJs", "clickJs", "selectJs", "scrollRef", "scrollPage", "hasText"} {
		if jsSections[name] == "" {
			t.Errorf("missing page script %s", name)
		}
	}
	// A value containing another placeholder is inserted as is, not substituted again.
	got := js("selectJs", "__REF__", jsStr(`"__WANT__"`), "__WANT__", jsStr("x"))
	if !strings.Contains(got, `.get("\"__WANT__\"")`) || strings.Contains(got, "__REF__") {
		t.Errorf("selectJs:\n%s", got)
	}
}

func TestControlHandoff(t *testing.T) {
	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	a, _ := envPeer(t, peers, "unix", false)
	b, _ := envPeer(t, peers, "tcp", false)

	// The agent seats itself per action and leaves the seat free after.
	ctx, epoch, end, err := s.beginAgent(context.Background(), browserGuard{})
	if err != nil || s.ctl.who != "agent" {
		t.Fatalf("beginAgent: %v, %+v", err, s.ctl)
	}
	if blocked, taken := end(epoch); blocked != "" || taken || s.ctl.who != "" || ctx.Err() == nil {
		t.Errorf("end: %q %v %+v", blocked, taken, s.ctl)
	}
	before := s.ctl.epoch

	// A human taking the seat mid-action cancels it and the action learns why.
	ctx, epoch, end, _ = s.beginAgent(context.Background(), browserGuard{})
	if err := s.setControl(a.ID, true); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Error("the agent's action was not cancelled")
	}
	if s.ctl.epoch <= epoch || s.ctl.who != "human" || s.ctl.peer != a.ID {
		t.Errorf("ctl = %+v", s.ctl)
	}
	if _, taken := end(epoch); !taken || s.ctl.who != "human" {
		t.Errorf("end after takeover: taken=%v ctl=%+v", taken, s.ctl)
	}
	if s.ctl.epoch <= before {
		t.Error("epoch did not advance")
	}

	// With a human in the seat, the agent fails at once, a second human can't take it, and a
	// stranger's release changes nothing.
	if _, _, _, err := s.beginAgent(context.Background(), browserGuard{}); !errors.Is(err, errHumanHasIt) || err.Error() != "the user has taken control of the browser; wait for them to release it" {
		t.Errorf("agent while human controls: %v", err)
	}
	if err := s.setControl(b.ID, true); err == nil {
		t.Error("two controllers")
	}
	_ = s.setControl(b.ID, false)
	if s.ctl.who != "human" || s.ctl.peer != a.ID {
		t.Errorf("a stranger's release: %+v", s.ctl)
	}
	if err := s.setControl(a.ID, true); err != nil {
		t.Errorf("taking again: %v", err)
	}

	// Releasing lets go of held buttons and hands the seat back.
	x, y := 5.0, 5.0
	s.input1(a.ID, protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "left", X: &x, Y: &y})
	s.input1(b.ID, protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "right", X: &x, Y: &y}) // not the controller: dropped
	if s.pressed.buttons != 1 {
		t.Errorf("held buttons = %d", s.pressed.buttons)
	}
	eventually(t, "the press to reach Chrome", func() bool { return f.has("Input.dispatchMouseEvent", `"type":"mousePressed"`) })
	epoch = s.ctl.epoch
	s.DropPeer(a.ID)
	if s.ctl.who != "" || s.ctl.epoch <= epoch {
		t.Errorf("after the controller dropped: %+v", s.ctl)
	}
	eventually(t, "the button to be released", func() bool {
		return f.has("Input.dispatchMouseEvent", `"button":"left"`) && f.has("Input.dispatchMouseEvent", `"type":"mouseReleased"`)
	})
	if f.has("Input.dispatchMouseEvent", `"button":"right"`) {
		t.Error("a non-controller's input reached the page")
	}
	if _, _, _, err := s.beginAgent(context.Background(), browserGuard{}); err != nil {
		t.Errorf("agent after release: %v", err)
	}
}

func TestBrowseFailsFastWhenHumanControls(t *testing.T) {
	peers := NewPeerRegistry()
	s, _ := fakeBrowser(t, peers)
	s.launched = nil
	human, _ := envPeer(t, peers, "unix", false)
	if err := s.setControl(human.ID, true); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := s.Browse(context.Background(), "chat", "snapshot", json.RawMessage(`{"action":"snapshot"}`), browserGuard{})
	if !errors.Is(err, errHumanHasIt) || time.Since(start) > time.Second {
		t.Errorf("Browse = %v after %v", err, time.Since(start))
	}
}

// Watching is open to everyone; operating is not, whatever the peer sends.
func TestScope(t *testing.T) {
	watcher := Access{Tier: TierLimited, Caps: capsFromMap(map[string]any{CapBrowser: browserWatch})}
	for _, ev := range []protocol.EventType{protocol.EventBrowserView, protocol.EventBrowserFrameAck} {
		if ok, _ := checkEvent(connDeps{}, watcher, &protocol.Envelope{Type: ev}); !ok {
			t.Errorf("%s should be allowed to a restricted peer that may watch", ev)
		}
		if tierMay(TierLimited, ev) {
			t.Errorf("%s should need the browser cap", ev)
		}
	}
	for _, ev := range []protocol.EventType{protocol.EventBrowserControl, protocol.EventBrowserInput, protocol.EventBrowserNav, protocol.EventBrowserFrame, protocol.EventBrowserState} {
		if ok, _ := checkEvent(connDeps{}, watcher, &protocol.Envelope{Type: ev}); ok {
			t.Errorf("%s must not be allowed to a restricted peer", ev)
		}
	}

	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	b := newBrowserBroker(peers)
	b.server = s
	phone, phoneEv := envPeer(t, peers, "tcp", true)
	owner, ownerEv := envPeer(t, peers, "unix", false)
	remoteFull, _ := envPeer(t, peers, "tcp", false)
	cfg, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Users["watcher"] = User{ID: "watcher", Tier: TierLimited, Caps: map[string]any{CapBrowser: browserWatch}}
	cfg.Users["boss"] = User{ID: "boss", Tier: TierFull}
	cfg.Tokens["phone-cred"] = TokenRecord{UserID: "watcher"}
	cfg.Tokens["full-cred"] = TokenRecord{UserID: "boss"}
	phone.TokenName, remoteFull.TokenName = "phone-cred", "full-cred"
	deps := connDeps{peers: peers, browser: b, settings: cfg}
	for _, ev := range []protocol.EventType{protocol.EventBrowserControl, protocol.EventBrowserInput, protocol.EventBrowserNav} {
		if !remoteDenied(deps, "tcp", phone.ID, ev) || remoteDenied(deps, "tcp", remoteFull.ID, ev) || remoteDenied(deps, "unix", owner.ID, ev) {
			t.Errorf("%s: wrong gate", ev)
		}
	}
	for _, ev := range []protocol.EventType{protocol.EventBrowserView, protocol.EventBrowserFrameAck} {
		if remoteDenied(deps, "tcp", phone.ID, ev) {
			t.Errorf("%s denied to a phone", ev)
		}
	}

	// Past the gate, the handler refuses a restricted peer too.
	drain(phoneEv)
	for _, ev := range []protocol.EventType{protocol.EventBrowserControl, protocol.EventBrowserInput, protocol.EventBrowserNav} {
		b.serverEvent(phone.ipc, phone.ID, &protocol.Envelope{Type: ev, Data: json.RawMessage(`{"take":true,"kind":"text","text":"x","action":"reload"}`)})
	}
	if s.ctl.who != "" || len(s.input) != 0 || f.count("Page.reload") != 0 {
		t.Errorf("a restricted peer operated the browser: %+v", s.ctl)
	}
	if got := countEvents(phoneEv, protocol.EventError); got != 3 {
		t.Errorf("errors sent = %d", got)
	}

	// ...but it can watch: it gets a state without canOperate, the owner with.
	b.serverEvent(phone.ipc, phone.ID, &protocol.Envelope{Type: protocol.EventBrowserView, Data: json.RawMessage(`{"on":true,"width":800,"height":600}`)})
	if st := lastState(t, phoneEv, func(protocol.BrowserStateData) bool { return true }); !st.Available || st.CanOperate || st.YouControl || st.Controller != "" {
		t.Errorf("phone state: %+v", st)
	}
	if _, ok := s.viewers[phone.ID]; !ok {
		t.Error("the phone is not a viewer")
	}
	b.serverEvent(owner.ipc, owner.ID, &protocol.Envelope{Type: protocol.EventBrowserControl, Data: json.RawMessage(`{"take":true}`)})
	drain(phoneEv)
	s.mu.Lock()
	s.used = true
	s.mu.Unlock()
	s.pushState()
	st := lastState(t, ownerEv, func(s protocol.BrowserStateData) bool { return s.YouControl })
	if st.Controller != "human" || !st.CanOperate {
		t.Errorf("owner state: %+v", st)
	}
	st = lastState(t, phoneEv, func(s protocol.BrowserStateData) bool { return s.Controller == "human" })
	if st.YouControl || st.CanOperate {
		t.Errorf("phone state while owner controls: %+v", st)
	}
}

func countEvents(ch chan *protocol.Envelope, typ protocol.EventType) int {
	n := 0
	for {
		select {
		case env := <-ch:
			if env.Type == typ {
				n++
			}
		case <-time.After(100 * time.Millisecond):
			return n
		}
	}
}

func TestViewerPacing(t *testing.T) {
	sent := make(chan uint64, 16)
	v := &viewer{send: func(f *frame) error { sent <- f.seq; return nil }}
	next := func() uint64 {
		select {
		case seq := <-sent:
			return seq
		case <-time.After(time.Second):
			t.Fatal("nothing sent")
			return 0
		}
	}
	none := func() {
		t.Helper()
		select {
		case seq := <-sent:
			t.Fatalf("frame %d sent with one in flight", seq)
		case <-time.After(50 * time.Millisecond):
		}
	}

	for seq := uint64(1); seq <= 5; seq++ {
		v.offer(&frame{seq: seq})
	}
	if got := next(); got != 1 {
		t.Fatalf("first frame = %d", got)
	}
	none() // 2..5 wait; only the newest survives
	v.ack(99)
	none() // an ack for a frame that isn't in flight changes nothing
	v.ack(1)
	if got := next(); got != 5 {
		t.Errorf("after the ack the viewer got %d, want the newest (5)", got)
	}
	v.ack(5)
	none() // nothing pending: idle
	v.offer(&frame{seq: 6})
	if got := next(); got != 6 {
		t.Errorf("idle viewer got %d", got)
	}

	// A viewer that never acks isn't stuck for ever.
	v.offer(&frame{seq: 7})
	none()
	v.mu.Lock()
	v.sentAt = time.Now().Add(-2 * viewerAckTimeout)
	v.mu.Unlock()
	v.offer(&frame{seq: 8})
	if got := next(); got != 8 {
		t.Errorf("after the ack timeout got %d", got)
	}

	// A failed write frees the slot.
	bad := &viewer{send: func(*frame) error { return errors.New("gone") }}
	bad.offer(&frame{seq: 1})
	eventually(t, "failed write to free the slot", func() bool {
		bad.mu.Lock()
		defer bad.mu.Unlock()
		return bad.inflight == 0
	})
}

// Frames fan out to every viewer at its own pace, and Chromium is acked for every one.
func TestFrameFanOut(t *testing.T) {
	s, f := fakeBrowser(t, NewPeerRegistry())
	fast := make(chan uint64, 32)
	slow := make(chan uint64, 32)
	vf := &viewer{send: func(fr *frame) error { fast <- fr.seq; return nil }}
	vs := &viewer{send: func(fr *frame) error { slow <- fr.seq; return nil }}
	s.viewers["fast"], s.viewers["slow"] = vf, vs
	s.cast = &cast{sid: "S1", w: 100, h: 100, q: 60}

	frameEv := func(id int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"data":"AAAA","sessionId":%d,"metadata":{}}`, id))
	}
	for id := 1; id <= 6; id++ {
		s.onFrame("S1", frameEv(id))
		if id == 1 {
			eventually(t, "first frames", func() bool { return len(fast) == 1 && len(slow) == 1 })
			vf.ack(<-fast) // the fast viewer keeps up with the next one
		}
	}
	s.onFrame("OTHER", frameEv(7)) // a frame of a tab that isn't streamed is acked and dropped
	eventually(t, "every frame acked to Chromium", func() bool { return f.count("Page.screencastFrameAck") == 7 })
	for id := 1; id <= 7; id++ {
		if !f.has("Page.screencastFrameAck", fmt.Sprintf(`{"sessionId":%d}`, id)) {
			t.Errorf("frame %d never acked to Chromium", id)
		}
	}
	if len(slow) != 1 {
		t.Errorf("the slow viewer has %d frames on the wire", len(slow))
	}
	if got := <-fast; got != 2 {
		t.Errorf("the fast viewer's next frame = %d", got)
	}
	vs.ack(<-slow)
	if got := <-slow; got != 6 {
		t.Errorf("the slow viewer jumped to %d, want the newest (6)", got)
	}

	// Oversized frames are dropped; the quality comes down, then the picture shrinks, and a new
	// viewer or another tab starts over.
	s.mu.Lock()
	s.qualSid = "S1"
	s.mu.Unlock()
	big := fmt.Sprintf(`{"data":%q,"sessionId":8}`, strings.Repeat("A", maxFrameData+4))
	for i := 0; i < 6; i++ {
		s.onFrame("S1", json.RawMessage(big))
	}
	state := func() (int, int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.quality, s.shrink
	}
	eventually(t, "quality and size to come down", func() bool { q, sh := state(); return q == 30 && sh < 100 })
	s.mu.Lock()
	s.active.sid = "S1"
	s.mu.Unlock()
	owner, _ := envPeer(t, s.peers, "unix", false)
	s.onView(owner.ID, protocol.BrowserViewData{On: true, Width: 800, Height: 600})
	if q, sh := state(); q != 60 || sh != 100 {
		t.Errorf("a new viewer starts at quality %d, size %d%%", q, sh)
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	p, _ := envPeer(t, peers, "unix", false)
	q, _ := envPeer(t, peers, "unix", false)

	s.onView(p.ID, protocol.BrowserViewData{On: true, Width: 10, Height: 99999}) // clamped
	if s.vp != (viewport{320, 1920}) {
		t.Errorf("viewport = %+v", s.vp)
	}
	if s.cast == nil || s.cast.sid != "S1" || !f.has("Page.startScreencast", `"maxHeight":1920`) {
		t.Errorf("screencast not started: %+v", s.cast)
	}
	s.onView(q.ID, protocol.BrowserViewData{On: true, Width: 800, Height: 600})
	if s.vp != (viewport{320, 1920}) {
		t.Errorf("a second viewer resized the page: %+v", s.vp)
	}
	s.onView(p.ID, protocol.BrowserViewData{On: false})
	s.onView(q.ID, protocol.BrowserViewData{On: false})
	if s.cast != nil || f.count("Page.stopScreencast") == 0 {
		t.Errorf("screencast still running with nobody watching: %+v", s.cast)
	}
	s.onView(p.ID, protocol.BrowserViewData{On: true, Width: 640, Height: 480})
	s.DropPeer(p.ID)
	if s.cast != nil || len(s.viewers) != 0 {
		t.Errorf("a dropped peer is still watched: %+v %v", s.cast, s.viewers)
	}
}

func TestRateLimit(t *testing.T) {
	s, _ := fakeBrowser(t, NewPeerRegistry())
	ok := 0
	for i := 0; i < 1000; i++ {
		if s.allow("p") {
			ok++
		}
	}
	if ok < inputRate || ok > inputRate+20 {
		t.Errorf("a burst of 1000 let %d through, want about %d", ok, inputRate)
	}
	if !s.allow("other") {
		t.Error("one peer's burst limited another")
	}
	time.Sleep(100 * time.Millisecond)
	if !s.allow("p") {
		t.Error("the bucket never refills")
	}
}

func TestBrokerHostChoice(t *testing.T) {
	peers := NewPeerRegistry()
	b := newBrowserBroker(peers)
	ctx := context.Background()
	args := json.RawMessage(`{"action":"snapshot"}`)

	// No desktop host and no server browser (Windows): the old explanation.
	if _, err := b.Browse(ctx, "chat", "snapshot", args); err != errNoBrowserHost {
		t.Fatalf("no backend: %v", err)
	}
	// No desktop host: the server's browser runs it. (Its launch fails here, and says why.)
	s := newServerBrowser(peers, t.TempDir(), func() []uint16 { return nil })
	s.launch = func(context.Context, []uint16, func(int64, int64)) (cdp, error) {
		return nil, errors.New("refusing to run the browser as root")
	}
	b.server = s
	if _, err := b.Browse(ctx, "chat", "snapshot", args); err == nil || !strings.Contains(err.Error(), "refusing to run the browser as root") {
		t.Errorf("server backend: %v", err)
	}
	if s.bootErr == "" {
		t.Error("the launch failure is not shown to viewers")
	}
	// A desktop host wins: the request goes to it, not to the server's browser.
	host, reqs := browserPeer(t, peers, "unix")
	peers.SetBrowserHost(host.ID, true)
	go func() {
		req := <-reqs
		b.Respond(host.ID, protocol.BrowserResponseData{RequestID: req.RequestID, Output: "from the desktop"})
	}()
	if out, err := b.Browse(ctx, "chat", "snapshot", args); err != nil || out.Output != "from the desktop" {
		t.Errorf("with a desktop host: %+v, %v", out, err)
	}
	// Where the server browser is unsupported (or absent: Windows) a viewer is told so.
	p, ev := envPeer(t, peers, "tcp", true)
	view := &protocol.Envelope{Type: protocol.EventBrowserView, Data: json.RawMessage(`{"on":true,"width":800,"height":600}`)}
	s.supports = false
	b.serverEvent(p.ipc, p.ID, view)
	if st := lastState(t, ev, func(protocol.BrowserStateData) bool { return true }); st.Available || st.Error == "" {
		t.Errorf("unsupported state: %+v", st)
	}
	b.server = nil
	b.serverEvent(p.ipc, p.ID, view)
	if st := lastState(t, ev, func(protocol.BrowserStateData) bool { return true }); st.Available || st.Error == "" {
		t.Errorf("no-backend state: %+v", st)
	}
}

func TestEnsureReportsSlowInstall(t *testing.T) {
	old := browserBootWait
	browserBootWait = 100 * time.Millisecond
	defer func() { browserBootWait = old }()
	s := newServerBrowser(NewPeerRegistry(), t.TempDir(), func() []uint16 { return nil })
	release := make(chan struct{})
	s.launch = func(_ context.Context, _ []uint16, progress func(int64, int64)) (cdp, error) {
		progress(30<<20, 120<<20)
		<-release
		return nil, errors.New("stopped")
	}
	defer close(release)
	// The install carries on after the caller gives up, and a retry joins it rather than starting another.
	for i := 0; i < 2; i++ {
		err := s.ensure(context.Background())
		if err == nil || err.Error() != "installing Chrome (30 of 120 MB); try again in a minute" {
			t.Errorf("ensure #%d = %v", i, err)
		}
	}
}

func TestIdleShutdown(t *testing.T) {
	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	s.idle = time.Minute
	p, _ := envPeer(t, peers, "tcp", true)
	closedNow := func() bool { return closed(f) }

	s.lastUse = time.Now().Add(-time.Hour)
	s.viewers[p.ID] = &viewer{}
	s.idleCheck()
	if closedNow() {
		t.Fatal("closed while someone watches")
	}
	delete(s.viewers, p.ID)
	s.ctl = control{who: "human", peer: p.ID}
	s.idleCheck()
	if closedNow() {
		t.Fatal("closed while a person holds control")
	}
	s.ctl = control{}
	s.lastUse = time.Now()
	s.idleCheck()
	if closedNow() {
		t.Fatal("closed right after use")
	}
	s.lastUse = time.Now().Add(-time.Hour)
	s.idleCheck()
	if !closedNow() {
		t.Error("an idle browser stayed open")
	}
}

func TestViewportRules(t *testing.T) {
	peers := NewPeerRegistry()
	s, _ := fakeBrowser(t, peers)
	watcher, _ := envPeer(t, peers, "tcp", true)
	operator, _ := envPeer(t, peers, "unix", false)
	view := func(p *Peer, w, h int) { s.onView(p.ID, protocol.BrowserViewData{On: true, Width: w, Height: h}) }
	vp := func() viewport { s.mu.Lock(); defer s.mu.Unlock(); return s.vp }

	view(watcher, 500, 400)
	if vp() != (viewport{defaultVPW, defaultVPH}) {
		t.Errorf("a watch-only sole viewer resized the page: %+v", vp())
	}
	delete(s.viewers, watcher.ID)
	s.mu.Lock()
	s.ctl = control{who: "agent"}
	s.mu.Unlock()
	view(operator, 500, 400)
	if vp() != (viewport{defaultVPW, defaultVPH}) {
		t.Errorf("a viewer resized the page mid-action: %+v", vp())
	}
	s.mu.Lock()
	s.ctl = control{}
	s.mu.Unlock()
	view(operator, 500, 400)
	if vp() != (viewport{500, 400}) {
		t.Errorf("an operator alone should resize: %+v", vp())
	}
	view(watcher, 700, 700) // a second viewer, not the controller
	view(operator, 600, 450)
	if vp() != (viewport{500, 400}) {
		t.Errorf("an operator sharing the panel without the seat resized: %+v", vp())
	}
}

func TestViewRateLimit(t *testing.T) {
	s, _ := fakeBrowser(t, NewPeerRegistry())
	ok := 0
	for i := 0; i < 200; i++ {
		if s.allowView("p") {
			ok++
		}
	}
	if ok < 10 || ok > 12 {
		t.Errorf("a burst of 200 view events let %d through", ok)
	}
}

// A device demoted to watch-only loses the seat, and a holder who can't operate (or has gone
// quiet) never locks the agent out.
func TestSeatReclaimed(t *testing.T) {
	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	holder := func(token string) *Peer {
		r, w := io.Pipe()
		t.Cleanup(func() { _ = w.Close() })
		go io.Copy(io.Discard, r)
		return peers.Add(protocol.NewIPCStream(nil, w), nopCloser{}, "tcp", peerIdentity{TokenName: token})
	}
	x, y := 5.0, 5.0

	p := holder("tok")
	peers.OnDemote = func(id string) { _ = s.setControl(id, false) }
	_ = s.setControl(p.ID, true)
	s.input1(p.ID, protocol.BrowserInputData{Kind: "mouse", Type: "down", Button: "left", X: &x, Y: &y})
	peers.SyncAccess(func(string) (string, string) { return TierLimited, "u" })
	if s.ctl.who != "" {
		t.Errorf("seat kept by a demoted device: %+v", s.ctl)
	}
	eventually(t, "held button released", func() bool { return f.has("Input.dispatchMouseEvent", `"type":"mouseReleased"`) })

	// Without the hook (or before it runs) the lazy checks still treat the holder as gone.
	peers.OnDemote = nil
	q := holder("tok2")
	_ = s.setControl(q.ID, true)
	peers.SyncAccess(func(string) (string, string) { return TierLimited, "u" })
	if st := s.stateFor(q); st.YouControl || st.Controller != "" {
		t.Errorf("state still shows the demoted holder: %+v", st)
	}
	_ = s.setControl(q.ID, false)

	r := holder("tok3")
	_ = s.setControl(r.ID, true)
	if err := s.humanNav(r.ID, protocol.BrowserNavData{Action: "navigate", URL: "file:///x"}); err == nil {
		t.Error("policy not applied")
	}
	s.mu.Lock()
	s.ctlAt = time.Now().Add(-2 * humanSeatIdle)
	s.mu.Unlock()
	_, _, end, err := s.beginAgent(context.Background(), browserGuard{})
	if err != nil {
		t.Fatalf("agent locked out by an idle holder: %v", err)
	}
	end(s.ctl.epoch)
}

func TestHeldKeysCapped(t *testing.T) {
	st := &inputState{vpW: 800, vpH: 600}
	key := func(i int, typ string) error {
		_, err := st.apply(protocol.BrowserInputData{Kind: "key", Type: typ, Key: "F1", Code: fmt.Sprintf("K%d", i)})
		return err
	}
	for i := 0; i < maxHeldKeys; i++ {
		if err := key(i, "down"); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if key(maxHeldKeys, "down") == nil {
		t.Error("a 33rd held key was accepted")
	}
	if key(3, "down") != nil {
		t.Error("repeating a held key (auto-repeat) was refused")
	}
	_ = key(0, "up")
	if key(maxHeldKeys, "down") != nil {
		t.Error("no room after a key came up")
	}
	if n := len(st.releaseAll()); n != maxHeldKeys {
		t.Errorf("released %d keys", n)
	}
}

func TestInputScale(t *testing.T) {
	x, y := 100.0, 50.0
	st := &inputState{vpW: 800, vpH: 600, scale: 2}
	cmds, err := st.apply(protocol.BrowserInputData{Kind: "mouse", Type: "move", X: &x, Y: &y})
	if err != nil || cmds[0].Params["x"] != 200.0 || cmds[0].Params["y"] != 100.0 {
		t.Errorf("shrunk picture: %v, %v", cmds, err)
	}
	cmds, _ = st.apply(protocol.BrowserInputData{Kind: "wheel", X: &x, Y: &y, DY: 10})
	if cmds[0].Params["deltaY"] != 20.0 {
		t.Errorf("wheel: %v", cmds)
	}
}

// A browser nobody started is started by an operator's panel, once; a watcher's doesn't. A seat
// taken before first use still tells everyone.
func TestFreshServerBrowser(t *testing.T) {
	peers := NewPeerRegistry()
	s := newServerBrowser(peers, t.TempDir(), func() []uint16 { return nil })
	var mu sync.Mutex
	launches := 0
	s.launch = func(context.Context, []uint16, func(int64, int64)) (cdp, error) {
		mu.Lock()
		launches++
		mu.Unlock()
		return nil, errors.New("no chrome here")
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return launches }
	watcher, _ := envPeer(t, peers, "tcp", true)
	owner, ownerEv := envPeer(t, peers, "unix", false)

	s.onView(watcher.ID, protocol.BrowserViewData{On: true, Width: 800, Height: 600})
	time.Sleep(100 * time.Millisecond)
	if count() != 0 {
		t.Fatal("a watcher started the browser")
	}
	_ = s.setControl(owner.ID, true)
	if st := lastState(t, ownerEv, func(st protocol.BrowserStateData) bool { return st.YouControl }); st.Controller != "human" {
		t.Errorf("state before first use: %+v", st)
	}
	s.onView(owner.ID, protocol.BrowserViewData{On: true, Width: 800, Height: 600})
	eventually(t, "the operator's panel to start the browser", func() bool { return count() == 1 })
	eventually(t, "the failure to be shown", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.bootErr != "" })
	s.onView(owner.ID, protocol.BrowserViewData{On: true, Width: 900, Height: 600})
	time.Sleep(100 * time.Millisecond)
	if count() != 1 {
		t.Errorf("a failed launch was retried by a resize (%d launches)", count())
	}
}

func TestTabOwnership(t *testing.T) {
	peers := NewPeerRegistry()
	s, _ := fakeBrowser(t, peers)
	mk := func(id, owner, url string) *tab {
		tb := &tab{targetID: "T" + id, sid: "S" + id, owner: owner, url: url, title: "page " + id}
		s.tabs = append(s.tabs, tb)
		s.byTarget[tb.targetID], s.bySession[tb.sid] = tb, tb
		return tb
	}
	a := mk("A", "chatA", "https://bank.example/")
	b := mk("B", "chatB", "https://other.example/")
	a2 := mk("A2", "chatA", "https://bank.example/2")
	ctx := context.Background()

	if got, _ := s.tabFor(ctx, "chatB"); got != b {
		t.Errorf("chat B got %+v", got)
	}
	if s.active != b {
		t.Error("acting did not make the tab the active one")
	}
	if got, _ := s.tabFor(ctx, "chatA"); got != a {
		t.Errorf("chat A got %+v", got)
	}
	s.active = a2
	if got, _ := s.tabFor(ctx, "chatA"); got != a2 {
		t.Error("a chat's active tab should win")
	}
	if l := s.tabList("chatB"); strings.Contains(l, "bank") || !strings.Contains(l, "1. (selected)") && !strings.Contains(l, "1. page B") {
		t.Errorf("chat B's list: %s", l)
	}
	if l := s.tabList("chatA"); strings.Contains(l, "other.example") || !strings.Contains(l, "2. (selected) page A2") {
		t.Errorf("chat A's list: %s", l)
	}
	if _, err := s.tabAt(1, "chatB"); err == nil {
		t.Error("chat B reached tab 2")
	}
	if got, err := s.tabAt(2, anyOwner); err != nil || got != b {
		t.Errorf("a person sees every tab: %v %v", got, err)
	}
	if n := len(s.tabSessions("chatB")); n != 1 {
		t.Errorf("chat B's tabs = %d", n)
	}
	// A blank tab nobody owns is claimed by the first chat to act; a used one never is.
	blank := s.tabs[0] // the tab the fake started with
	mk("D", "", "https://person.example/")
	if got, _ := s.tabFor(ctx, "chatC"); got != blank || blank.owner != "chatC" {
		t.Errorf("chat C got %+v", got)
	}
}

func TestRefusedTellsTheNavigator(t *testing.T) {
	peers := NewPeerRegistry()
	s, _ := fakeBrowser(t, peers)
	p, ev := envPeer(t, peers, "unix", false)
	s.mu.Lock()
	s.navBy, s.navAt = p.ID, time.Now()
	s.mu.Unlock()
	s.refused(blockedScheme("file:///etc/passwd"))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-ev:
			var d protocol.ErrorData
			if env.Type == protocol.EventError && json.Unmarshal(env.Data, &d) == nil && strings.Contains(d.Message, "file:///etc/passwd") {
				return
			}
		case <-deadline:
			t.Fatal("the person who navigated was not told")
		}
	}
}

// A tab's owner is fixed at creation: a blank-looking tab with history isn't up for grabs, and
// one chat can't fill the browser.
func TestTabOwnerIsFixed(t *testing.T) {
	peers := NewPeerRegistry()
	s, f := fakeBrowser(t, peers)
	blank := s.tabs[0]
	f.visited = 2 // a person went somewhere and came back to about:blank
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if got, err := s.tabFor(ctx, "chatB"); err == nil && got == blank {
		t.Error("chat B took a tab with the person's history")
	}
	if blank.owner != "" {
		t.Errorf("owner = %q", blank.owner)
	}
	f.visited = 0
	if got, err := s.tabFor(context.Background(), "chatB"); err != nil || got != blank || blank.owner != "chatB" {
		t.Errorf("a pristine tab wasn't claimed: %v %v", got, err)
	}

	if _, err := s.Browse(context.Background(), "", "snapshot", json.RawMessage(`{"action":"snapshot"}`), browserGuard{}); err == nil {
		t.Error("an empty session id was accepted")
	}

	for i := 0; i < maxOwnerTabs; i++ {
		tb := &tab{targetID: fmt.Sprint("X", i), sid: fmt.Sprint("SX", i), owner: "chatB"}
		s.tabs = append(s.tabs, tb)
	}
	if _, err := s.newTab(context.Background(), "chatB"); err == nil || !strings.Contains(err.Error(), "too many tabs") {
		t.Errorf("per-chat tab cap: %v", err)
	}
}

func TestAgentResize(t *testing.T) {
	s, f := fakeBrowser(t, NewPeerRegistry())
	ctx := context.Background()
	browse := func(w, h int) error {
		raw, _ := json.Marshal(map[string]any{"action": "resize", "width": w, "height": h})
		_, err := s.Browse(ctx, "chat", "resize", raw, browserGuard{})
		return err
	}
	if err := browse(390, 844); err != nil {
		t.Fatal(err)
	}
	if !f.has("Emulation.setDeviceMetricsOverride", `"height":844,"mobile":false,"width":390`) {
		t.Errorf("the page was not resized")
	}
	for _, bad := range [][2]int{{0, 0}, {100, 800}, {1280, 5000}} {
		if browse(bad[0], bad[1]) == nil {
			t.Errorf("resize %v should be refused", bad)
		}
	}
}

func TestDevtoolsActions(t *testing.T) {
	s, f := fakeBrowser(t, NewPeerRegistry())
	ctx := context.Background()
	run := func(a map[string]any) (string, error) {
		raw, _ := json.Marshal(a)
		res, err := s.Browse(ctx, "chat", a["action"].(string), raw, browserGuard{})
		return res.Output, err
	}

	// network: requests are listed, filtered, and redirect hops keep their own rows.
	ev := func(method, body string) { s.onNetwork("S1", method, json.RawMessage(body)) }
	ev("Network.requestWillBeSent", `{"requestId":"1","type":"Document","request":{"url":"https://a.test/","method":"GET"}}`)
	ev("Network.responseReceived", `{"requestId":"1","response":{"status":200}}`)
	ev("Network.loadingFinished", `{"requestId":"1","encodedDataLength":2048}`)
	ev("Network.requestWillBeSent", `{"requestId":"2","type":"Fetch","request":{"url":"https://a.test/api","method":"POST"}}`)
	ev("Network.loadingFailed", `{"requestId":"2","errorText":"net::ERR_CONNECTION_REFUSED"}`)
	ev("Network.requestWillBeSent", `{"requestId":"3","type":"Fetch","request":{"url":"https://a.test/ignored","method":"GET"}}`)
	ev("Network.responseReceived", `{"requestId":"3","response":{"status":404}}`)
	ev("Network.loadingFailed", `{"requestId":"3","errorText":"net::ERR_ABORTED"}`)
	out, _ := run(map[string]any{"action": "network"})
	for _, want := range []string{"200 GET document https://a.test/ 2.0 KB", "FAILED ERR_CONNECTION_REFUSED POST fetch", "404 GET fetch https://a.test/ignored"} {
		if !strings.Contains(out, want) {
			t.Errorf("network missing %q:\n%s", want, out)
		}
	}
	if out, _ = run(map[string]any{"action": "network", "filter": "/api"}); strings.Contains(out, "a.test/ ") {
		t.Errorf("filter kept the document:\n%s", out)
	}
	for i := 0; i < maxNetReqs+50; i++ {
		ev("Network.requestWillBeSent", fmt.Sprintf(`{"requestId":"x%d","request":{"url":"https://a.test/%d","method":"GET"}}`, i, i))
	}
	if n, m := len(s.tabs[0].net), len(s.tabs[0].netByID); n != maxNetReqs || m != maxNetReqs {
		t.Errorf("network log not bounded: %d rows, %d ids", n, m)
	}

	// emulate: metrics, touch and user agent change; a reload follows.
	if _, err := run(map[string]any{"action": "emulate", "device": "iphone"}); err != nil {
		t.Fatal(err)
	}
	if !f.has("Emulation.setDeviceMetricsOverride", `"height":844,"mobile":true,"width":390`) || !f.has("Emulation.setUserAgentOverride", "iPhone") || f.count("Page.reload") != 1 {
		t.Errorf("emulate did not apply: %v", f.calls)
	}

	// click options.
	cmds := clickCmdsWith(5, 6, "right", true)
	if len(cmds) != 5 || !strings.Contains(fmt.Sprint(cmds[3].Params), "clickCount:2") || !strings.Contains(fmt.Sprint(cmds[1].Params), "button:right") {
		t.Errorf("right double click: %v", cmds)
	}

	// upload refuses what isn't an absolute, readable file before touching the page.
	for _, files := range [][]string{{"relative.txt"}, {"/nonexistent/file"}, {t.TempDir()}} {
		if _, err := run(map[string]any{"action": "upload", "ref": "1", "files": files}); err == nil {
			t.Errorf("upload %v should fail", files)
		}
	}
}
