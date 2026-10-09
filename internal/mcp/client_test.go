package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// exercise runs the same checks against any connected client.
func exercise(t *testing.T, c *Client, wantProto string) {
	t.Helper()
	ctx := ctx5(t)
	if got := c.Info().Protocol; got != wantProto {
		t.Fatalf("protocol = %q, want %q", got, wantProto)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 7 {
		t.Fatalf("got %d tools across two pages, want 7: %+v", len(tools), tools)
	}
	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil || res.IsError || len(res.Content) != 1 || res.Content[0].Text != "hello" {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	res, err = c.CallTool(ctx, "fail", nil)
	if err != nil || !res.IsError || res.Content[0].Text != "it broke" {
		t.Fatalf("fail = %+v, %v (a tool error is a result, not a transport error)", res, err)
	}
	if _, err := c.CallTool(ctx, "nope", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown tool error = %v", err)
	}
	if _, err := c.CallTool(ctx, "echo", json.RawMessage(`[1]`)); err == nil {
		t.Fatal("arguments that aren't an object must be refused")
	}
	rs, err := c.ListResources(ctx)
	if err != nil || len(rs) != 1 || rs[0].URI != "mem://a" {
		t.Fatalf("resources = %+v, %v", rs, err)
	}
	blocks, err := c.ReadResource(ctx, "mem://a")
	if err != nil || len(blocks) != 1 || blocks[0].Text != "alpha" {
		t.Fatalf("read = %+v, %v", blocks, err)
	}
}

func TestStdioLegacy(t *testing.T) {
	c, err := Dial(ctx5(t), Options{Config: stdioConfig("legacy", nil), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exercise(t, c, "2025-11-25")
	if c.Info().Name != "fake" || c.Info().Instructions != "Be nice." {
		t.Fatalf("info = %+v", c.Info())
	}
}

func TestStdioModern(t *testing.T) {
	c, err := Dial(ctx5(t), Options{Config: stdioConfig("modern", nil), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exercise(t, c, protoModern)
	if c.Info().Name != "fake-modern" {
		t.Fatalf("info = %+v", c.Info())
	}
}

func TestStdioEnvironmentIsIsolated(t *testing.T) {
	t.Setenv("YOAKE_SECRET", "hunter2")
	c, err := Dial(ctx5(t), Options{Config: stdioConfig("legacy", map[string]string{"FROM_CONFIG": "yes"}), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.CallTool(ctx5(t), "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := res.Content[0].Text
	if strings.Contains(env, "YOAKE_SECRET") || strings.Contains(env, "hunter2") {
		t.Fatal("the daemon's environment leaked into the server")
	}
	if !strings.Contains(env, "FROM_CONFIG=yes") || !strings.Contains(env, "PATH=") {
		t.Fatalf("the server lacks its configured and base environment:\n%s", env)
	}
}

func TestStdioStartFailureAndExit(t *testing.T) {
	if _, err := Dial(ctx5(t), Options{Config: ServerConfig{Command: "/no/such/binary-xyz"}, Dir: t.TempDir()}); err == nil {
		t.Fatal("a missing command must fail")
	}
	// A process that exits straight away is a failed connect that says so.
	_, err := Dial(ctx5(t), Options{Config: ServerConfig{Command: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}}, Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the server's stderr in it", err)
	}
}

func TestStdioCancelTellsTheServer(t *testing.T) {
	c, err := Dial(ctx5(t), Options{Config: stdioConfig("legacy", nil), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.CallTool(ctx, "slow", nil); done <- err }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStdioCloseKillsTheProcess(t *testing.T) {
	c, err := Dial(ctx5(t), Options{Config: stdioConfig("legacy", nil), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the process outlived Close")
	}
}

func TestHTTPLegacyWithSessionAndSSE(t *testing.T) {
	h := newHTTPFake(t, "legacy", true, true, "")
	c, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: h.srv.URL, Headers: map[string]string{"X-Api-Key": "k1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exercise(t, c, "2025-11-25")
	h.mu.Lock()
	defer h.mu.Unlock()
	last := h.headers[len(h.headers)-1]
	if last.Get("Mcp-Session-Id") != "sess-1" || last.Get("MCP-Protocol-Version") != "2025-11-25" || last.Get("X-Api-Key") != "k1" {
		t.Fatalf("headers on a later request = %v", last)
	}
}

func TestHTTPSessionExpiryStartsANewSession(t *testing.T) {
	h := newHTTPFake(t, "legacy", true, false, "")
	c, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: h.srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.t.(*httpTransport).setSession("stale") // the server no longer knows this one
	res, err := c.CallTool(ctx5(t), "echo", json.RawMessage(`{"text":"again"}`))
	if err != nil || res.Content[0].Text != "again" {
		t.Fatalf("after expiry = %+v, %v", res, err)
	}
}

func TestHTTPModern(t *testing.T) {
	h := newHTTPFake(t, "modern", false, true, "")
	c, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: h.srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exercise(t, c, protoModern)
	h.mu.Lock()
	defer h.mu.Unlock()
	var sawName bool
	for _, hd := range h.headers {
		if hd.Get("Mcp-Method") == "tools/call" && hd.Get("Mcp-Name") == "echo" {
			sawName = true
		}
		if hd.Get("Mcp-Session-Id") != "" {
			t.Fatal("the modern revision has no sessions")
		}
	}
	if !sawName {
		t.Fatal("Mcp-Method and Mcp-Name are required on a modern tools/call")
	}
}

func TestHTTPNeedsAuth(t *testing.T) {
	h := newHTTPFake(t, "legacy", false, false, "secret")
	_, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: h.srv.URL}})
	var auth *errNeedsAuth
	if !errors.As(err, &auth) {
		t.Fatalf("err = %v, want errNeedsAuth", err)
	}
	if !strings.HasSuffix(auth.Resource, "/.well-known/oauth-protected-resource") || auth.Scope != "read" {
		t.Fatalf("challenge = %+v", auth)
	}
	// With a token it connects.
	c, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: h.srv.URL}, Bearer: func(context.Context) (string, error) { return "secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestHTTPRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect was followed to another host")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: srv.URL, Headers: map[string]string{"X-Api-Key": "k"}}})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want a refused redirect", err)
	}
}

func TestHTTPNotAnMCPServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := Dial(ctx5(t), Options{Config: ServerConfig{URL: srv.URL}}); err == nil {
		t.Fatal("a server that isn't MCP must fail to connect")
	}
}

// legacySSEFake is the 2024-11-05 HTTP+SSE transport.
func legacySSEFake(t *testing.T, endpointHost string) *httptest.Server {
	t.Helper()
	f := &fakeServer{era: "legacy"}
	events := make(chan string, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		ep := "/messages?s=1"
		if endpointHost != "" {
			ep = endpointHost + "/messages"
		}
		_, _ = w.Write([]byte("event: endpoint\ndata: " + ep + "\n\n"))
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-events:
				_, _ = w.Write([]byte("event: message\ndata: " + ev + "\n\n"))
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var m message
		_ = json.NewDecoder(r.Body).Decode(&m)
		w.WriteHeader(http.StatusAccepted)
		if resp := f.handle(&m); resp != nil {
			b, _ := json.Marshal(resp)
			events <- string(b)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLegacySSETransport(t *testing.T) {
	srv := legacySSEFake(t, "")
	c, err := Dial(ctx5(t), Options{Config: ServerConfig{Type: "sse", URL: srv.URL + "/sse"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exercise(t, c, "2025-11-25")
}

func TestLegacySSEEndpointMustStayOnTheServer(t *testing.T) {
	srv := legacySSEFake(t, "http://169.254.169.254")
	_, err := Dial(ctx5(t), Options{Config: ServerConfig{Type: "sse", URL: srv.URL + "/sse"}})
	if err == nil || !strings.Contains(err.Error(), "another site") {
		t.Fatalf("err = %v, want the foreign endpoint refused", err)
	}
}

func TestReadSSE(t *testing.T) {
	in := ": keep-alive\n\nevent: a\ndata: one\ndata: two\n\nid: 7\ndata: three\n\ndata: tail"
	var got []sseEvent
	if err := readSSE(strings.NewReader(in), func(e sseEvent) bool { got = append(got, e); return true }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Event != "a" || got[0].Data != "one\ntwo" || got[1].ID != "7" || got[2].Data != "tail" {
		t.Fatalf("events = %+v", got)
	}
}
