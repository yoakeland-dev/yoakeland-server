package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// sseTransport is the HTTP+SSE transport of the 2024-11-05 revision, kept for
// servers that never moved off it: the client holds one GET open, the server's
// first event names the URL to POST to, and every response comes back on the
// GET stream.
type sseTransport struct {
	url      string
	hc       *http.Client
	headers  map[string]string
	bearer   bearerFunc
	h        handler
	endpoint string

	cancel  context.CancelFunc
	mu      sync.Mutex
	pending map[string]chan *message
	closed  chan struct{}
	readErr error
	once    sync.Once
}

// connectSSE opens the stream and waits for the server to say where to POST.
func connectSSE(ctx context.Context, rawURL string, headers map[string]string, bearer bearerFunc, h handler) (*sseTransport, error) {
	t := &sseTransport{url: rawURL, hc: newHTTPClient(rawURL), headers: headers, bearer: bearer, h: h,
		pending: map[string]chan *message{}, closed: make(chan struct{})}
	streamCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := t.decorate(ctx, req); err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.hc.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		defer resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusUnauthorized {
			res, scope := parseChallenge(resp.Header.Values("WWW-Authenticate"))
			return nil, &errNeedsAuth{Resource: res, Scope: scope}
		}
		return nil, &httpError{Status: resp.StatusCode}
	}
	ready := make(chan error, 1)
	go t.read(resp.Body, ready)
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		cancel()
		return nil, errors.New("the server never said where to send requests")
	}
	return t, nil
}

func (t *sseTransport) decorate(ctx context.Context, req *http.Request) error {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.bearer != nil && req.Header.Get("Authorization") == "" {
		tok, err := t.bearer(ctx)
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return nil
}

func (t *sseTransport) read(body io.ReadCloser, ready chan<- error) {
	defer body.Close()
	signalled := false
	err := readSSE(body, func(ev sseEvent) bool {
		switch ev.Event {
		case "endpoint":
			if signalled {
				return true
			}
			base, _ := url.Parse(t.url)
			ref, err := url.Parse(strings.TrimSpace(ev.Data))
			if err != nil || base == nil {
				ready <- fmt.Errorf("the server sent a bad endpoint %q", ev.Data)
				signalled = true
				return false
			}
			target := base.ResolveReference(ref)
			// The endpoint is where requests, and the credentials on them, go: it
			// has to stay on the server the user pointed at.
			if !sameOrigin(base, target) {
				ready <- fmt.Errorf("the server named an endpoint on another site (%s)", target.Host)
				signalled = true
				return false
			}
			t.endpoint = target.String()
			ready <- nil
			signalled = true
		case "message", "":
			msgs, err := parseMessages([]byte(ev.Data))
			if err != nil {
				return true
			}
			for _, m := range msgs {
				t.dispatch(m)
			}
		}
		return true
	})
	if !signalled {
		if err == nil {
			err = errors.New("the stream ended before the server named an endpoint")
		}
		ready <- err
	}
	t.mu.Lock()
	t.readErr = err
	t.mu.Unlock()
	t.once.Do(func() { close(t.closed) })
}

func (t *sseTransport) dispatch(m *message) {
	switch {
	case m.isResponse():
		t.mu.Lock()
		ch := t.pending[idKey(m.ID)]
		delete(t.pending, idKey(m.ID))
		t.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	case m.isRequest():
		go func() {
			if reply := t.h(m); reply != nil {
				_ = t.send(context.Background(), reply)
			}
		}()
	default:
		t.h(m)
	}
}

func (t *sseTransport) send(ctx context.Context, m *message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := t.decorate(ctx, req); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			res, scope := parseChallenge(resp.Header.Values("WWW-Authenticate"))
			return &errNeedsAuth{Resource: res, Scope: scope}
		}
		return &httpError{Status: resp.StatusCode, Body: snippet(b)}
	}
	return nil
}

func (t *sseTransport) call(ctx context.Context, req *message) (*message, error) {
	ch := make(chan *message, 1)
	key := idKey(req.ID)
	t.mu.Lock()
	t.pending[key] = ch
	t.mu.Unlock()
	forget := func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}
	if err := t.send(ctx, req); err != nil {
		forget()
		return nil, err
	}
	select {
	case m := <-ch:
		return m, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-t.closed:
		forget()
		return nil, errors.New("the server closed its event stream")
	}
}

func (t *sseTransport) notify(ctx context.Context, n *message) error { return t.send(ctx, n) }
func (t *sseTransport) done() <-chan struct{}                        { return t.closed }
func (t *sseTransport) diag() string                                 { return "" }

func (t *sseTransport) close() {
	t.cancel()
	t.once.Do(func() { close(t.closed) })
}
