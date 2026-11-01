package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Needs a real Chrome: skipped unless YOAKE_CHROME points at one, or
// YOAKE_CHROME_IT=1 asks for the pinned download (~120MB) into a temp dir.
// As root it also needs YOAKE_BROWSER_NO_SANDBOX=1.
func TestBrowserIntegration(t *testing.T) {
	if os.Getenv("YOAKE_CHROME") == "" && os.Getenv("YOAKE_CHROME_IT") != "1" {
		t.Skip("set YOAKE_CHROME or YOAKE_CHROME_IT=1 to run against a real Chrome")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	state := t.TempDir()
	bin, err := Ensure(ctx, state, func(done, total int64) {
		if done == total {
			t.Logf("downloaded %d bytes", done)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<title>hello yoake</title>")
	}))
	defer site.Close()
	ownPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<title>should be unreachable</title>")
	}))
	defer ownPort.Close()
	u, _ := url.Parse(ownPort.URL)
	var port uint16
	fmt.Sscan(u.Port(), &port)

	b, err := Launch(ctx, Options{Binary: bin, StateDir: state, BlockedPorts: []uint16{port}, Debugf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	call := func(sid, method string, params any, out any) {
		t.Helper()
		res, err := b.Call(ctx, sid, method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if out != nil {
			if err := json.Unmarshal(res, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var created struct{ TargetID string }
	call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &created)
	var attached struct{ SessionID string }
	call("", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached)
	sid := attached.SessionID
	call(sid, "Page.enable", nil, nil)

	navigate := func(to string) (errorText string) {
		var nav struct{ ErrorText string }
		call(sid, "Page.navigate", map[string]any{"url": to}, &nav)
		return nav.ErrorText
	}

	if e := navigate(site.URL); e != "" {
		t.Fatalf("navigating to the test server: %s", e)
	}
	var ev struct {
		Result struct{ Value string }
	}
	call(sid, "Runtime.evaluate", map[string]any{"expression": "document.title"}, &ev)
	if ev.Result.Value != "hello yoake" {
		t.Errorf("document.title = %q", ev.Result.Value)
	}

	const refused = "ERR_SOCKS_CONNECTION_FAILED"
	if e := navigate("http://169.254.169.254/"); !strings.Contains(e, refused) {
		t.Errorf("metadata navigation error = %q, want %s", e, refused)
	}
	if e := navigate(ownPort.URL); !strings.Contains(e, refused) {
		t.Errorf("own-port navigation error = %q, want %s", e, refused)
	}

	b.Close()
	select {
	case <-b.Done():
	default:
		t.Error("Done not closed after Close")
	}
}

// A Chrome that stops reading fd 3 must not wedge Close.
func TestCloseStalledPipe(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	r, w, _ := os.Pipe() // nobody reads r
	defer r.Close()
	b := &Browser{cmd: cmd, w: w, pending: map[int64]chan message{}, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(b.done) }()
	go b.Call(context.Background(), "", "X", map[string]string{"d": strings.Repeat("a", 1<<20)}) // blocks in Write
	time.Sleep(300 * time.Millisecond)
	closed := make(chan struct{})
	go func() { b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Close hung with a stalled pipe")
	}
}
