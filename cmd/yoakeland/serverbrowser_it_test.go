package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// envPeer registers a peer and returns every envelope sent to it.
func envPeer(t *testing.T, peers *PeerRegistry, network string, restricted bool) (*Peer, chan *protocol.Envelope) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	out := make(chan *protocol.Envelope, 4096)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, protocol.MaxFrameSize)
		for sc.Scan() {
			var env protocol.Envelope
			if json.Unmarshal(sc.Bytes(), &env) == nil {
				select {
				case out <- &env:
				default:
				}
			}
		}
	}()
	p := peers.Add(protocol.NewIPCStream(nil, w), nopCloser{}, network, peerIdentity{DeviceType: "web", Restricted: restricted})
	return p, out
}

func sendEnv(t *testing.T, sb *serverBrowser, p *Peer, typ protocol.EventType, data any) {
	t.Helper()
	raw, _ := json.Marshal(data)
	sb.handle(p.ipc, p.ID, &protocol.Envelope{Type: typ, Data: raw})
}

// Drives the server browser against a real Chrome: skipped unless YOAKE_CHROME points at one, or
// YOAKE_CHROME_IT=1 asks for the pinned download (~120MB) into a temp dir. As root it also needs
// YOAKE_BROWSER_NO_SANDBOX=1.
func TestServerBrowserIntegration(t *testing.T) {
	if os.Getenv("YOAKE_CHROME") == "" && os.Getenv("YOAKE_CHROME_IT") != "1" {
		t.Skip("set YOAKE_CHROME or YOAKE_CHROME_IT=1 to run against a real Chrome")
	}
	var site *httptest.Server
	var hits sync.Map // path -> request count
	hang := make(chan struct{})
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		switch r.URL.Path {
		case "/hang":
			<-hang
		case "/post":
			fmt.Fprint(w, `<title>posted</title>`)
		case "/redir":
			http.Redirect(w, r, "http://elsewhere.test:"+strings.Split(site.URL, ":")[2]+"/", http.StatusFound)
		case "/popup":
			fmt.Fprint(w, `<title>popup</title><a id=p href="/second" target="_blank">open</a>`)
		case "/second":
			fmt.Fprint(w, `<title>second page</title><p>arrived</p>`)
		default:
			fmt.Fprint(w, `<title>Home</title><h1>Hello</h1>
<button onclick="this.textContent='clicked'; console.error('boom')">Press</button>
<input aria-label="name" placeholder="name">
<a href="file:///etc/hostname">local file</a>
<select aria-label="pick"><option>one</option><option>two</option></select>
<script>setTimeout(()=>console.warn("careful"), 10)</script>`)
		}
	}))
	defer site.Close()
	defer close(hang) // first: a hung handler would stall Close

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	peers := NewPeerRegistry()
	sb := newServerBrowser(peers, t.TempDir(), nil)
	sb.ports = func() []uint16 { return nil }
	defer sb.Close()
	b := newBrowserBroker(peers)
	b.server = sb

	owner, ownerEv := envPeer(t, peers, "unix", false)
	phone, phoneEv := envPeer(t, peers, "tcp", true)

	act := func(a map[string]any) (string, error) {
		t.Helper()
		raw, _ := json.Marshal(a)
		res, err := b.Browse(ctx, "chat", a["action"].(string), raw)
		return res.Output, err
	}
	must := func(a map[string]any) string {
		t.Helper()
		out, err := act(a)
		if err != nil {
			t.Fatalf("%v: %v", a, err)
		}
		return out
	}
	ref := func(snap, label string) string {
		for _, l := range strings.Split(snap, "\n") {
			if strings.Contains(l, label) {
				if i := strings.Index(l, "[ref="); i >= 0 {
					return strings.TrimSuffix(l[i+5:], "]")
				}
			}
		}
		t.Fatalf("no %q in snapshot:\n%s", label, snap)
		return ""
	}

	// Viewers from the start, so the screencast runs. The operator sets the page size; a
	// watch-only phone doesn't.
	sendEnv(t, sb, owner, protocol.EventBrowserView, protocol.BrowserViewData{On: true, Width: 900, Height: 600, SessionID: "chat"})
	sendEnv(t, sb, phone, protocol.EventBrowserView, protocol.BrowserViewData{On: true, Width: 900, Height: 600})

	snap := must(map[string]any{"action": "navigate", "url": site.URL})
	for _, want := range []string{"Title: Home", "Hello", `button "Press"`} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, snap)
		}
	}
	if !strings.Contains(snap, "Viewport: 900x600") {
		t.Errorf("viewer size not applied:\n%s", snap)
	}

	snap = must(map[string]any{"action": "click", "ref": ref(snap, `button "Press"`)})
	if !strings.Contains(snap, `button "clicked"`) {
		t.Errorf("click did nothing:\n%s", snap)
	}
	if !strings.Contains(snap, "Console since the last snapshot") || !strings.Contains(snap, "boom") {
		t.Errorf("console error not reported:\n%s", snap)
	}

	snap = must(map[string]any{"action": "type", "ref": ref(snap, `textbox "name"`), "text": "yoake"})
	if !strings.Contains(snap, `value="yoake"`) {
		t.Errorf("type did nothing:\n%s", snap)
	}
	snap = must(map[string]any{"action": "select", "ref": ref(snap, `combobox "pick"`), "text": "two"})
	snap = must(map[string]any{"action": "press", "key": "Control+a"})

	if out := must(map[string]any{"action": "evaluate", "expression": "({n: 1+1, s: document.title})"}); !strings.Contains(out, `"n": 2`) {
		t.Errorf("evaluate = %q", out)
	}
	if out := must(map[string]any{"action": "tabs", "op": "list"}); !strings.HasPrefix(out, "Tabs:\n1. (selected) Home") {
		t.Errorf("tabs = %q", out)
	}

	raw, _ := json.Marshal(map[string]any{"action": "screenshot"})
	res, err := b.Browse(ctx, "chat", "screenshot", raw)
	if err != nil || res.Image == nil || res.Image.MimeType != "image/jpeg" || res.Image.Width != 900 || res.Image.Width > 1280 {
		t.Fatalf("screenshot: %v, %v", res.Image != nil, err)
	}

	// A file: URL never loads, whether the agent asks for it or the page does.
	if _, err := act(map[string]any{"action": "navigate", "url": "file:///etc/hostname"}); err == nil {
		t.Error("file: navigation worked")
	}
	snap = must(map[string]any{"action": "snapshot"})
	_, _ = act(map[string]any{"action": "click", "ref": ref(snap, `link "local file"`)})
	// (Chrome never offers a blob: load to the request filter; the committed URL is checked too.)
	_, _ = act(map[string]any{"action": "evaluate", "expression": `location.href = URL.createObjectURL(new Blob(["<p>leak</p>"], {type: "text/html"}))`})
	time.Sleep(time.Second)
	if out := must(map[string]any{"action": "snapshot"}); strings.Contains(out, "blob:") || strings.Contains(out, "leak") {
		t.Errorf("a non-web page loaded:\n%s", out)
	}
	must(map[string]any{"action": "navigate", "url": site.URL})

	// The cross-origin guard stops a redirect off the approved hosts; the browser stays put.
	b.blockCrossOrigin.Store(true)
	b.approve("chat", "navigate", json.RawMessage(`{"url":"`+site.URL+`/redir"}`))
	if _, err := act(map[string]any{"action": "navigate", "url": site.URL + "/redir"}); err == nil || !strings.Contains(err.Error(), "elsewhere.test") || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("redirect off-site: %v", err)
	}
	if out := must(map[string]any{"action": "snapshot"}); !strings.Contains(out, "Title: Home") {
		t.Errorf("browser did not stay on the page:\n%s", out)
	}

	// A load that fails is an error, as in the desktop panel.
	if _, err := act(map[string]any{"action": "navigate", "url": "http://127.0.0.1:1/"}); err == nil || !strings.Contains(err.Error(), "couldn't load the page") {
		t.Errorf("failed load: %v", err)
	}
	if _, err := act(map[string]any{"action": "reload"}); err == nil || !strings.Contains(err.Error(), "couldn't load the page") {
		t.Errorf("failed reload: %v", err)
	}
	must(map[string]any{"action": "navigate", "url": site.URL})

	// A target=_blank link opens in the same tab.
	must(map[string]any{"action": "navigate", "url": site.URL + "/popup"})
	snap = must(map[string]any{"action": "snapshot"})
	snap = must(map[string]any{"action": "click", "ref": ref(snap, `link "open"`)})
	if !strings.Contains(snap, "arrived") {
		t.Errorf("popup did not load in place:\n%s", snap)
	}
	if out := must(map[string]any{"action": "tabs", "op": "list"}); strings.Count(out, "\n") != 1 {
		t.Errorf("popup left a tab: %q", out)
	}

	// A new window loads once, in the opener's tab: the popup itself never makes a request.
	if n, _ := hits.Load("/second"); n.(*atomic.Int32).Load() != 1 {
		t.Errorf("the popup target was requested %d times", n.(*atomic.Int32).Load())
	}
	// A popup to a host the chat wasn't cleared for is stopped like any other navigation.
	must(map[string]any{"action": "navigate", "url": site.URL + "/popup"})
	if _, err := act(map[string]any{"action": "evaluate", "expression": `window.open("http://elsewhere.test:1/", "_blank") && 1`}); err == nil || !strings.Contains(err.Error(), "elsewhere.test") {
		t.Errorf("popup off-site: %v", err)
	}
	if out := must(map[string]any{"action": "tabs", "op": "list"}); strings.Count(out, "\n") != 1 {
		t.Errorf("popup left a tab: %q", out)
	}

	// A viewer got frames; keep acking until a fresh one shows the new page is being streamed.
	frames := 0
	deadline := time.After(10 * time.Second)
loop:
	for frames < 3 {
		select {
		case env := <-phoneEv:
			if env.Type != protocol.EventBrowserFrame {
				continue
			}
			var f protocol.BrowserFrameData
			_ = json.Unmarshal(env.Data, &f)
			if f.Width != 900 || f.Height != 600 || len(f.Data) == 0 {
				t.Fatalf("frame %d: %dx%d, %d bytes", f.Seq, f.Width, f.Height, len(f.Data))
			}
			frames++
			sendEnv(t, sb, phone, protocol.EventBrowserFrameAck, protocol.BrowserFrameAckData{Seq: f.Seq})
			if frames == 2 {
				must(map[string]any{"action": "scroll", "direction": "down"}) // make the page repaint
			}
		case <-deadline:
			break loop
		}
	}
	if frames < 2 {
		t.Errorf("viewer got %d frames", frames)
	}

	// Control: the phone is restricted and can't take it; the owner can, which stops the agent.
	drain(phoneEv)
	sendEnv(t, sb, phone, protocol.EventBrowserControl, protocol.BrowserControlData{Take: true})
	if sb.ctl.who != "" {
		t.Error("a restricted peer took control")
	}
	drain(ownerEv)
	sendEnv(t, sb, owner, protocol.EventBrowserControl, protocol.BrowserControlData{Take: true})
	if _, err := act(map[string]any{"action": "snapshot"}); err == nil || !strings.Contains(err.Error(), "taken control") {
		t.Errorf("agent while human controls: %v", err)
	}
	st := lastState(t, ownerEv, func(s protocol.BrowserStateData) bool { return s.YouControl })
	if st.Controller != "human" || !st.CanOperate {
		t.Errorf("owner state: %+v", st)
	}
	st = lastState(t, phoneEv, func(s protocol.BrowserStateData) bool { return s.Controller == "human" })
	if st.YouControl || st.CanOperate {
		t.Errorf("phone state: %+v", st)
	}
	// The owner navigates, clicks into the field and types by hand, then leaves.
	sendEnv(t, sb, owner, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: site.URL})
	var box struct{ X, Y float64 }
	for i := 0; i < 50 && box.X == 0; i++ {
		time.Sleep(100 * time.Millisecond)
		raw, err := sb.eval(ctx, sb.activeTab(), `(() => { const r = document.querySelector("input")?.getBoundingClientRect(); return r ? {x: r.left + 5, y: r.top + 5} : {x: 0, y: 0} })()`, false)
		if err == nil {
			_ = json.Unmarshal(raw, &box)
		}
	}
	if box.X == 0 {
		t.Fatal("the owner's navigation never landed")
	}
	for _, in := range []map[string]any{
		{"kind": "mouse", "type": "move", "x": box.X, "y": box.Y},
		{"kind": "mouse", "type": "down", "button": "left", "clicks": 1, "x": box.X, "y": box.Y},
		{"kind": "mouse", "type": "up", "button": "left", "clicks": 1, "x": box.X, "y": box.Y},
		{"kind": "text", "text": "hi!"},
		{"kind": "key", "type": "down", "key": "Backspace", "code": "Backspace"},
		{"kind": "key", "type": "up", "key": "Backspace", "code": "Backspace"},
	} {
		sendEnv(t, sb, owner, protocol.EventBrowserInput, in)
	}
	time.Sleep(500 * time.Millisecond)
	if raw, _ := sb.eval(ctx, sb.activeTab(), `document.querySelector("input").value`, false); string(raw) != `"hi"` {
		t.Errorf("typed value = %s", raw)
	}
	sb.DropPeer(owner.ID)
	if sb.ctl.who != "" {
		t.Errorf("control not released on disconnect: %+v", sb.ctl)
	}

	// A hanging load doesn't hold the browser: the next navigation goes through, and a refused
	// one is reported to the person who made it.
	drain(ownerEv)
	sendEnv(t, sb, owner, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: site.URL + "/hang"})
	time.Sleep(4 * time.Second) // past the moment the seat is let go
	sendEnv(t, sb, owner, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: "http://169.254.169.254/"})
	deadline = time.After(10 * time.Second)
	for got := false; !got; {
		select {
		case env := <-ownerEv:
			var d protocol.ErrorData
			if env.Type == protocol.EventError && json.Unmarshal(env.Data, &d) == nil {
				if strings.Contains(d.Message, "busy") {
					t.Fatalf("a hanging load blocked the next navigation: %s", d.Message)
				}
				got = strings.Contains(d.Message, "couldn't load the page")
			}
		case <-deadline:
			t.Fatal("no error for a navigation that failed")
		}
	}

	// Two chats share the browser but not tabs: B can't see or touch A's page.
	must(map[string]any{"action": "navigate", "url": site.URL})
	actB := func(a map[string]any) (string, error) {
		raw, _ := json.Marshal(a)
		res, err := b.Browse(ctx, "chat-b", a["action"].(string), raw)
		return res.Output, err
	}
	if out, err := actB(map[string]any{"action": "snapshot"}); err != nil || !strings.Contains(out, "URL: about:blank") || strings.Contains(out, "Hello") {
		t.Errorf("chat B read chat A's page: %q, %v", out, err)
	}
	if out, _ := actB(map[string]any{"action": "tabs", "op": "list"}); strings.Contains(out, site.URL) || strings.Count(out, "\n") != 1 {
		t.Errorf("chat B's tabs: %q", out)
	}
	if _, err := actB(map[string]any{"action": "tabs", "op": "select", "tab": 2}); err == nil {
		t.Error("chat B selected chat A's tab")
	}
	if _, err := actB(map[string]any{"action": "navigate", "url": site.URL + "/second"}); err != nil {
		t.Fatal(err)
	}
	if out := must(map[string]any{"action": "snapshot"}); !strings.Contains(out, "Title: Home") {
		t.Errorf("chat A lost its page:\n%s", out)
	}
	if out := must(map[string]any{"action": "tabs", "op": "list"}); strings.Contains(out, "second") || strings.Count(out, "\n") != 1 {
		t.Errorf("chat A's tabs: %q", out)
	}
	if st := lastState(t, phoneEv, func(st protocol.BrowserStateData) bool { return len(st.Tabs) == 2 }); !st.Tabs[0].Active && !st.Tabs[1].Active {
		t.Errorf("viewers see every tab: %+v", st.Tabs)
	}

	// A wide viewer: the agent's screenshot is still no wider than 1280.
	sendEnv(t, sb, phone, protocol.EventBrowserView, protocol.BrowserViewData{On: false})
	sendEnv(t, sb, owner, protocol.EventBrowserView, protocol.BrowserViewData{On: true, Width: 1700, Height: 900})
	res, err = b.Browse(ctx, "chat", "screenshot", raw)
	if err != nil || res.Image == nil || res.Image.Width != 1280 || res.Image.Height < 677 || res.Image.Height > 679 {
		t.Errorf("wide screenshot: %v, %v", res.Image != nil && res.Image.Width > 0, err)
	}

	// A person's page can't be taken over by a chat. (A) A viewer declaring another chat navigates
	// chat "chat"'s tab: they get a tab of their own. (B) A tab a person used, even one they
	// returned to about:blank, is never claimed, and "back" can't reach their history.
	owner2, _ := envPeer(t, peers, "unix", false)
	tabsOf := func(owner string) (n int) {
		sb.mu.Lock()
		defer sb.mu.Unlock()
		for _, tb := range sb.tabs {
			if tb.owner == owner {
				n++
			}
		}
		return n
	}
	urlOf := func(tb *tab) string { sb.mu.Lock(); defer sb.mu.Unlock(); return tb.url }
	waitURL := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if tb := sb.activeTab(); tb != nil && strings.HasPrefix(urlOf(tb), want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("the active tab never reached %s", want)
	}
	sendEnv(t, sb, owner2, protocol.EventBrowserView, protocol.BrowserViewData{On: true, Width: 800, Height: 600, SessionID: "chat-b"})
	sendEnv(t, sb, owner2, protocol.EventBrowserControl, protocol.BrowserControlData{Take: true})
	before := tabsOf("chat")
	sendEnv(t, sb, owner2, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: site.URL + "/second"})
	waitURL(site.URL + "/second")
	if tabsOf("chat") != before || tabsOf("chat-b") == 0 {
		t.Errorf("a viewer for chat-b retagged a tab: chat has %d (had %d), chat-b has %d", tabsOf("chat"), before, tabsOf("chat-b"))
	}
	sendEnv(t, sb, owner2, protocol.EventBrowserControl, protocol.BrowserControlData{Take: false})
	if out := must(map[string]any{"action": "snapshot"}); !strings.Contains(out, "Title: Home") {
		t.Errorf("chat lost its page to a viewer's navigation:\n%s", out)
	}
	if _, err := b.Browse(ctx, "chat-b", "back", json.RawMessage(`{"action":"back"}`)); err != nil && strings.Contains(err.Error(), "Home") {
		t.Errorf("back reached another chat's page: %v", err)
	}

	sb.Close()
	sendEnv(t, sb, owner2, protocol.EventBrowserView, protocol.BrowserViewData{On: true, Width: 800, Height: 600})
	sendEnv(t, sb, owner2, protocol.EventBrowserControl, protocol.BrowserControlData{Take: true})
	sendEnv(t, sb, owner2, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: site.URL})
	waitURL(site.URL)
	sendEnv(t, sb, owner2, protocol.EventBrowserNav, protocol.BrowserNavData{Action: "navigate", URL: "about:blank"})
	waitURL("about:blank")
	sendEnv(t, sb, owner2, protocol.EventBrowserControl, protocol.BrowserControlData{Take: false})
	personTab := sb.activeTab()
	rawB := func(action string) (tool_ string, err error) {
		res, err := b.Browse(ctx, "chat-b", action, json.RawMessage(`{"action":"`+action+`"}`))
		return res.Output, err
	}
	if out, err := rawB("snapshot"); err != nil || strings.Contains(out, "Hello") {
		t.Errorf("chat-b snapshot: %q, %v", out, err)
	}
	if bt, _ := sb.tabFor(ctx, "chat-b"); bt == personTab {
		t.Error("chat-b took the tab the person used")
	}
	if out, err := rawB("back"); err == nil && strings.Contains(out, "Hello") {
		t.Errorf("back reached the person's page:\n%s", out)
	}

	// A crashed or closed browser is relaunched by the next action.
	sb.Close()
	if out := must(map[string]any{"action": "snapshot"}); !strings.Contains(out, "URL: about:blank") {
		t.Errorf("after relaunch:\n%s", out)
	}
}

func drain(ch chan *protocol.Envelope) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// lastState waits for a browser_state satisfying ok.
func lastState(t *testing.T, ch chan *protocol.Envelope, ok func(protocol.BrowserStateData) bool) protocol.BrowserStateData {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-ch:
			var s protocol.BrowserStateData
			if env.Type == protocol.EventBrowserState && json.Unmarshal(env.Data, &s) == nil && ok(s) {
				return s
			}
		case <-deadline:
			t.Fatal("no matching browser_state")
		}
	}
}
