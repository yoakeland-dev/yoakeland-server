package tool

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchTool_Execute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>ignored</title></head><body>
			<article>
				<h1>Yoakeland Article</h1>
				<p>This is the main body text of the article, long enough for readability to keep it as the primary content block instead of discarding it as boilerplate.</p>
			</article>
			<nav>skip this navigation link list</nav>
		</body></html>`))
	}))
	defer srv.Close()

	// bypass the SSRF-guarded dialer: this test intentionally targets the
	// loopback httptest server, which the production dialer would refuse.
	ft := &WebFetchTool{client: srv.Client()}

	argsJSON, _ := json.Marshal(WebFetchArgs{URL: srv.URL})
	out, err := ft.Execute(context.Background(), ExecutionContext{}, argsJSON)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Yoakeland Article") || !strings.Contains(out, "main body text") {
		t.Errorf("output missing expected content: %q", out)
	}
	if strings.Contains(out, "skip this navigation") {
		t.Errorf("output should not include nav boilerplate: %q", out)
	}
}

func TestWebFetchTool_Execute_EmptyURL(t *testing.T) {
	ft := NewWebFetchTool()
	if _, err := ft.Execute(context.Background(), ExecutionContext{}, []byte(`{"url":"  "}`)); err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestWebFetchTool_Execute_BadScheme(t *testing.T) {
	ft := NewWebFetchTool()
	if _, err := ft.Execute(context.Background(), ExecutionContext{}, []byte(`{"url":"file:///etc/passwd"}`)); err == nil {
		t.Fatal("expected error for non-http(s) scheme")
	}
}

func TestWebFetchTool_Execute_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>should never be reached</body></html>"))
	}))
	defer srv.Close()

	ft := NewWebFetchTool() // real SSRF-guarded dialer

	argsJSON, _ := json.Marshal(WebFetchArgs{URL: srv.URL})
	if _, err := ft.Execute(context.Background(), ExecutionContext{}, argsJSON); err == nil {
		t.Fatal("expected error when fetching a loopback address")
	}
}

func TestIsBlockedFetchTarget(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.100.100.100":  true, // Tailscale / CGNAT
		"0.0.0.1":          true, // "this network" reaches localhost on Linux
		"::ffff:127.0.0.1": true, "::127.0.0.1": true, "64:ff9b::7f00:1": true, "2002:7f00:1::1": true,
		"198.18.0.1": true, "240.0.0.1": true, "fd00::1": true, "fe80::1": true,
		"93.184.216.34": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
	} {
		if got := isBlockedFetchTarget(net.ParseIP(ip)); got != want {
			t.Errorf("isBlockedFetchTarget(%s) = %v, want %v", ip, got, want)
		}
	}
}
