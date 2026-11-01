package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live checks of the devtools actions against a real Chrome (YOAKE_CHROME or YOAKE_CHROME_IT=1;
// YOAKE_IT_STATE reuses one state dir so Chrome is downloaded once).
func TestServerBrowserDevtoolsLive(t *testing.T) {
	if os.Getenv("YOAKE_CHROME") == "" && os.Getenv("YOAKE_CHROME_IT") != "1" {
		t.Skip("set YOAKE_CHROME or YOAKE_CHROME_IT=1 to run against a real Chrome")
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			w.WriteHeader(404)
		case "/up":
			fmt.Fprint(w, "ok")
		default:
			fmt.Fprint(w, `<title>Dev</title><meta name=viewport content="width=device-width"><body style="margin:0">
<button id=b>Target</button><div id=log></div>
<input type=file aria-label="doc" id=f onchange="log.textContent='uploaded:'+this.files[0].name">
<div id=track style="position:relative;width:300px;height:20px;background:#ccc"><div id=knob role=button tabindex=0 aria-label="knob" style="position:absolute;left:0;width:20px;height:20px;background:red"></div></div>
<div id=goal role=button tabindex=0 aria-label="goal" style="margin-left:250px;width:40px;height:20px;background:blue"></div>
<p id=ua></p><p id=w></p>
<div style="height:3000px;background:linear-gradient(red,blue)">tall</div>
<script>
ua.textContent=navigator.userAgent; w.textContent='w='+innerWidth+' touch='+('ontouchstart' in window);
fetch('/api'); 
b.addEventListener('contextmenu',e=>{e.preventDefault();log.textContent='right'});
b.addEventListener('dblclick',()=>log.textContent='double');
let drag=false;knob.addEventListener('mousedown',()=>drag=true);
addEventListener('mousemove',e=>{if(drag)knob.style.left=Math.min(280,e.clientX)+'px'});
addEventListener('mouseup',()=>{if(drag){drag=false;log.textContent='dragged:'+knob.style.left}});
</script>`)
		}
	}))
	defer site.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	state := os.Getenv("YOAKE_IT_STATE")
	if state == "" {
		state = t.TempDir()
	}
	peers := NewPeerRegistry()
	sb := newServerBrowser(peers, state, func() []uint16 { return nil })
	defer sb.Close()
	b := newBrowserBroker(peers)
	b.server = sb

	act := func(a map[string]any) (string, *string, error) {
		t.Helper()
		raw, _ := json.Marshal(a)
		res, err := b.Browse(ctx, "chat", a["action"].(string), raw)
		var img *string
		if res.Image != nil {
			img = &res.Image.Data
			if res.Image.Width <= 0 {
				t.Errorf("%v: image has no size", a)
			}
		}
		return res.Output, img, err
	}
	must := func(a map[string]any) (string, *string) {
		t.Helper()
		out, img, err := act(a)
		if err != nil {
			t.Fatalf("%v: %v", a, err)
		}
		return out, img
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

	snap, _ := must(map[string]any{"action": "navigate", "url": site.URL})
	if strings.Contains(snap, "HeadlessChrome") {
		t.Errorf("user agent still says HeadlessChrome:\n%s", snap)
	}

	// network
	time.Sleep(300 * time.Millisecond)
	out, _ := must(map[string]any{"action": "network"})
	if !strings.Contains(out, "200 GET document") || !strings.Contains(out, "404 GET fetch") {
		t.Errorf("network:\n%s", out)
	}
	if out, _ = must(map[string]any{"action": "network", "filter": "/api"}); strings.Contains(out, "document") {
		t.Errorf("network filter:\n%s", out)
	}

	// click options
	btn := ref(snap, `"Target"`)
	if snap, _ = must(map[string]any{"action": "click", "ref": btn, "button": "right"}); !strings.Contains(snap, "right") {
		t.Errorf("right click:\n%s", snap)
	}
	if snap, _ = must(map[string]any{"action": "click", "ref": btn, "double": true}); !strings.Contains(snap, "double") {
		t.Errorf("double click:\n%s", snap)
	}

	// drag
	snap, _ = must(map[string]any{"action": "snapshot"})
	if snap, _ = must(map[string]any{"action": "drag", "ref": ref(snap, `"knob"`), "to": ref(snap, `"goal"`)}); !strings.Contains(snap, "dragged:") {
		t.Errorf("drag:\n%s", snap)
	}

	// upload
	file := filepath.Join(t.TempDir(), "hello.txt")
	_ = os.WriteFile(file, []byte("hi"), 0o600)
	snap, _ = must(map[string]any{"action": "snapshot"})
	if snap, _ = must(map[string]any{"action": "upload", "ref": ref(snap, `"doc"`), "files": []string{file}}); !strings.Contains(snap, "uploaded:hello.txt") {
		t.Errorf("upload:\n%s", snap)
	}
	if _, _, err := act(map[string]any{"action": "upload", "ref": btn, "files": []string{file}}); err == nil {
		t.Errorf("upload to a button should fail")
	}

	// screenshots: viewport, element, full page differ in height
	size := func(a map[string]any) int {
		_, img := must(a)
		raw, _ := base64.StdEncoding.DecodeString(*img)
		_, h := jpegSize(base64.StdEncoding.EncodeToString(raw), 0, 0)
		return h
	}
	snap, _ = must(map[string]any{"action": "snapshot"})
	view := size(map[string]any{"action": "screenshot"})
	full := size(map[string]any{"action": "screenshot", "full_page": true})
	el := size(map[string]any{"action": "screenshot", "ref": ref(snap, `"goal"`)})
	if !(full > view && el > 0 && el < view) {
		t.Errorf("screenshot heights: viewport %d, full page %d, element %d", view, full, el)
	}

	// resize + emulate
	if snap, _ = must(map[string]any{"action": "resize", "width": 500, "height": 400}); !strings.Contains(snap, "Viewport: 500x400") {
		t.Errorf("resize:\n%s", snap)
	}
	snap, _ = must(map[string]any{"action": "emulate", "device": "iphone"})
	for _, want := range []string{"Viewport: 390x844", "iPhone", "touch=true"} {
		if !strings.Contains(snap, want) {
			t.Errorf("emulate missing %q:\n%s", want, snap)
		}
	}
	if snap, _ = must(map[string]any{"action": "emulate", "device": "desktop"}); !strings.Contains(snap, "Viewport: 1280x800") || strings.Contains(snap, "iPhone") {
		t.Errorf("emulate desktop:\n%s", snap)
	}
}
