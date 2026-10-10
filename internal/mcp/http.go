package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errNeedsAuth means the server answered 401: it wants a sign-in. Resource is
// the protected-resource metadata URL its WWW-Authenticate header named, if any.
type errNeedsAuth struct {
	Resource string
	Scope    string
	Detail   string
}

func (e *errNeedsAuth) Error() string {
	if e.Detail != "" {
		return "the server needs sign-in (" + e.Detail + ")"
	}
	return "the server needs sign-in"
}

// errSessionExpired: the server forgot the session (HTTP 404 on a request that
// carried an Mcp-Session-Id), so the client must start a new one.
var errSessionExpired = errors.New("the server's session expired")

// httpError is a status the transport has no meaning for.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// bearerFunc supplies an OAuth access token ("" when there is none).
type bearerFunc func(ctx context.Context) (string, error)

// httpTransport speaks Streamable HTTP: every message is its own POST, and
// the answer to a request comes back as one JSON document or an SSE stream.
type httpTransport struct {
	url     string
	hc      *http.Client
	headers map[string]string
	bearer  bearerFunc
	h       handler

	mu      sync.Mutex
	session string // Mcp-Session-Id, legacy servers only
	version string // MCP-Protocol-Version to send once negotiated
	modern  bool   // add the per-request headers the 2026-07-28 revision requires
	closed  chan struct{}
	once    sync.Once
}

func newHTTP(rawURL string, headers map[string]string, bearer bearerFunc, h handler) *httpTransport {
	return &httpTransport{url: rawURL, hc: newHTTPClient(rawURL), headers: headers, bearer: bearer, h: h, closed: make(chan struct{})}
}

// newHTTPClient is a client with no overall timeout (a response may stream for
// as long as a tool runs; callers bound it with a context) that refuses to
// follow a redirect to another host: a header the user set for one server
// (an API key) must not travel to wherever that server points next.
func newHTTPClient(origin string) *http.Client {
	start, _ := url.Parse(origin)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 0,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if start != nil && !sameOrigin(start, req.URL) {
				return fmt.Errorf("refusing a redirect to another site (%s)", req.URL.Host)
			}
			return nil
		},
	}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (t *httpTransport) setSession(id string) {
	t.mu.Lock()
	t.session = id
	t.mu.Unlock()
}

func (t *httpTransport) setVersion(v string, modern bool) {
	t.mu.Lock()
	t.version, t.modern = v, modern
	t.mu.Unlock()
}

func (t *httpTransport) sessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}

// newRequest builds a POST of m with every header a request needs.
func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte, m *message, accept string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	t.mu.Lock()
	session, version, modern := t.session, t.version, t.modern
	t.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if modern && m != nil && m.Method != "" {
		req.Header.Set("Mcp-Method", m.Method)
		if name := mcpName(m); name != "" {
			req.Header.Set("Mcp-Name", headerValue(name))
		}
	}
	if t.bearer != nil && req.Header.Get("Authorization") == "" {
		tok, err := t.bearer(ctx)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return req, nil
}

// mcpName is the Mcp-Name header's source: a tool or prompt name, or a resource URI.
func mcpName(m *message) string {
	switch m.Method {
	case "tools/call", "prompts/get", "resources/read":
		var p struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Name != "" {
			return p.Name
		}
		return p.URI
	}
	return ""
}

// headerValue sends a value as is when it is plain ASCII, else base64 in the
// =?base64?...?= form the spec defines, so it can't carry a line break.
func headerValue(v string) string {
	plain := v != "" && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?="))
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			plain = false
		}
	}
	if strings.TrimSpace(v) != v {
		plain = false
	}
	if plain {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

func (t *httpTransport) call(ctx context.Context, req *message) (*message, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := t.newRequest(ctx, http.MethodPost, body, req, "application/json, text/event-stream")
	if err != nil {
		return nil, err
	}
	resp, err := t.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := t.checkStatus(resp); err != nil {
		return nil, err
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" && req.Method == "initialize" {
		t.setSession(id)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var got *message
	take := func(m *message) bool {
		switch {
		case m.isResponse():
			if idKey(m.ID) == idKey(req.ID) {
				got = m
				return false
			}
		case m.isRequest():
			if reply := t.h(m); reply != nil {
				go t.post(context.WithoutCancel(ctx), reply)
			}
		default:
			t.h(m)
		}
		return true
	}
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		err = readSSE(resp.Body, func(ev sseEvent) bool {
			if ev.Event != "" && ev.Event != "message" {
				return true
			}
			msgs, perr := parseMessages([]byte(ev.Data))
			if perr != nil {
				return true
			}
			for _, m := range msgs {
				if !take(m) {
					return false
				}
			}
			return true
		})
		if err != nil && got == nil {
			return nil, fmt.Errorf("reading the response stream: %w", err)
		}
	default:
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxFrame+1))
		if rerr != nil {
			return nil, rerr
		}
		if len(data) > maxFrame {
			return nil, errors.New("the response is too large")
		}
		msgs, perr := parseMessages(data)
		if perr != nil {
			return nil, fmt.Errorf("the server's reply isn't JSON-RPC (%s): %w", snippet(data), perr)
		}
		for _, m := range msgs {
			if !take(m) {
				break
			}
		}
	}
	if got == nil {
		return nil, errors.New("the server ended its response without answering")
	}
	return got, nil
}

func (t *httpTransport) notify(ctx context.Context, n *message) error {
	return t.post(ctx, n)
}

// post sends a message that expects no answer beyond the status.
func (t *httpTransport) post(ctx context.Context, m *message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	hreq, err := t.newRequest(ctx, http.MethodPost, body, m, "application/json, text/event-stream")
	if err != nil {
		return err
	}
	resp, err := t.hc.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return t.checkStatus(resp)
}

// checkStatus turns a non-success status into the error that says what to do.
func (t *httpTransport) checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		res, scope := parseChallenge(resp.Header.Values("WWW-Authenticate"))
		return &errNeedsAuth{Resource: res, Scope: scope, Detail: snippet(body)}
	case http.StatusNotFound:
		if t.sessionID() != "" {
			return errSessionExpired
		}
	}
	return &httpError{Status: resp.StatusCode, Body: snippet(body)}
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(strings.ToValidUTF8(string(b), "?")), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// parseChallenge reads resource_metadata and scope out of Bearer challenges.
func parseChallenge(values []string) (resource, scope string) {
	for _, v := range values {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "bearer") {
			continue
		}
		resource = challengeParam(v, "resource_metadata")
		scope = challengeParam(v, "scope")
		return
	}
	return
}

func challengeParam(header, name string) string {
	i := strings.Index(header, name+"=")
	if i < 0 {
		return ""
	}
	rest := header[i+len(name)+1:]
	if strings.HasPrefix(rest, `"`) {
		rest = rest[1:]
		if j := strings.Index(rest, `"`); j >= 0 {
			return rest[:j]
		}
		return ""
	}
	if j := strings.IndexAny(rest, ", "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// listen holds the GET stream a legacy server may offer for the messages it
// starts itself (a changed tool list). A server without one answers 405, and
// that is the end of it.
func (t *httpTransport) listen(ctx context.Context) {
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-t.closed:
				return
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		req, err := t.newRequest(ctx, http.MethodGet, nil, nil, "text/event-stream")
		if err != nil {
			return
		}
		resp, err := t.hc.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Body.Close()
			return
		}
		attempt = 0
		_ = readSSE(resp.Body, func(ev sseEvent) bool {
			msgs, perr := parseMessages([]byte(ev.Data))
			if perr != nil {
				return true
			}
			for _, m := range msgs {
				switch {
				case m.isRequest():
					if reply := t.h(m); reply != nil {
						go t.post(context.WithoutCancel(ctx), reply)
					}
				case m.isNotify():
					t.h(m)
				}
			}
			return true
		})
		resp.Body.Close()
	}
}

func (t *httpTransport) done() <-chan struct{} { return t.closed }
func (t *httpTransport) diag() string          { return "" }

// close ends the session, if the server kept one, and stops the listener.
func (t *httpTransport) close() {
	t.once.Do(func() {
		close(t.closed)
		if t.sessionID() == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := t.newRequest(ctx, http.MethodDelete, nil, nil, "application/json")
		if err != nil {
			return
		}
		if resp, err := t.hc.Do(req); err == nil {
			resp.Body.Close()
		}
	})
}
