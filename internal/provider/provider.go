package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

type Provider interface {
	StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition, systemPrompt string) (<-chan agent.ModelStreamChunk, <-chan error)
}

type SSEEvent struct {
	Event string
	Data  string
}

// ReadSSEStream decodes standard text/event-stream chunks line by line
func ReadSSEStream(r io.Reader, handle func(ev SSEEvent) error) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 8*1024*1024)

	var currentEvent SSEEvent
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if currentEvent.Data != "" || currentEvent.Event != "" {
				if err := handle(currentEvent); err != nil {
					return err
				}
				currentEvent = SSEEvent{}
			}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataVal := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentEvent.Data == "" {
				currentEvent.Data = dataVal
			} else {
				currentEvent.Data += "\n" + dataVal
			}
		}
	}

	// A read error (or ErrTooLong) mid-event is an error, not a clean end.
	if err := scanner.Err(); err != nil {
		return err
	}
	if currentEvent.Data != "" || currentEvent.Event != "" {
		return handle(currentEvent)
	}
	return nil
}

// rateLimitBackoff is the wait before each retry of a transient failure; a var
// so tests can shrink it. A Retry-After header from the provider wins over it.
var rateLimitBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

const maxRetryAfter = 30 * time.Second

// streamIdleTimeout is how long an HTTP stream may go without a byte before
// it counts as dead. Generous: a reasoning model can think silently for minutes,
// and a local server may still be loading the model. A var so tests can shrink it.
var streamIdleTimeout = 5 * time.Minute

// retryableStatus is whether a response is worth another try: a rate limit,
// a server error, or Anthropic's 529 "overloaded".
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// doWithRetry sends the request newReq builds, retrying retryable statuses
// with backoff. Retrying is safe because nothing has been streamed yet: the
// status comes back before any body.
func doWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil || !retryableStatus(resp.StatusCode) || attempt >= len(rateLimitBackoff) {
			return resp, err
		}
		wait := rateLimitBackoff[attempt]
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs >= 0 {
			wait = min(time.Duration(secs)*time.Second, maxRetryAfter)
		}
		resp.Body.Close()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// stallReader fails a body read once the stream has gone idle, so a server
// that stops talking mid-reply ends the turn with an error instead of pinning
// it until the user cancels.
type stallReader struct {
	body    io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallReader(body io.ReadCloser, idle time.Duration) *stallReader {
	s := &stallReader{body: body, idle: idle}
	s.timer = time.AfterFunc(idle, func() {
		s.stalled.Store(true)
		_ = body.Close()
	})
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.timer.Reset(s.idle)
	n, err := s.body.Read(p)
	if s.stalled.Load() {
		return n, fmt.Errorf("stream stalled: no data for %s", s.idle)
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.timer.Stop()
	return s.body.Close()
}

// errorMessage is the text of a provider's "error" value, which is either an
// object with a message or a bare string.
func errorMessage(raw json.RawMessage) string {
	var obj struct {
		Message string `json:"message"`
	}
	var s string
	switch {
	case json.Unmarshal(raw, &obj) == nil && obj.Message != "":
		return obj.Message
	case json.Unmarshal(raw, &s) == nil:
		return s
	}
	return ""
}

// httpError is the error for a non-200 response. It carries the provider's
// message when the body has one, else the start of the body: an HTML 502
// page, say, is still worth showing. Reads at most 4 KiB.
func httpError(resp *http.Response, label string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(raw))
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Error) > 0 {
		if m := errorMessage(body.Error); m != "" {
			msg = m
		}
	}
	return fmt.Errorf("%s error (%d): %s", label, resp.StatusCode, msg)
}
