package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

// collectStream drains a StreamTurn, returning its chunks and the stream error.
func collectStream(t *testing.T, chunks <-chan agent.ModelStreamChunk, errs <-chan error) ([]agent.ModelStreamChunk, error) {
	t.Helper()
	var got []agent.ModelStreamChunk
	var streamErr error
	for chunks != nil || errs != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			got = append(got, c)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				streamErr = err
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not finish")
		}
	}
	return got, streamErr
}

func sseServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
}

func openAIStream(t *testing.T, events ...string) ([]agent.ModelStreamChunk, error) {
	t.Helper()
	srv := sseServer(t, events...)
	t.Cleanup(srv.Close)
	p := NewOpenAI("k", "gpt-4o")
	p.BaseURL = srv.URL
	chunks, errs := p.StreamTurn(context.Background(), nil, []tool.ToolDefinition{{Name: "x"}}, "")
	return collectStream(t, chunks, errs)
}

// PRV-4: tool-call indexes need not start at 0 or be contiguous, and a
// backend that omits them must not merge two calls into one.
func TestOpenAIStream_ToolCallIndexesNeedNotBeContiguous(t *testing.T) {
	got, err := openAIStream(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"read","arguments":"{\"p\":"}}]}}]}`+"\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":3,"id":"b","function":{"name":"ls","arguments":"{}"}}]}}]}`+"\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"x\"}"}}]}}]}`+"\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var calls []*agent.ToolCall
	for _, c := range got {
		if c.Kind == agent.StreamChunkToolCall {
			calls = append(calls, c.ToolCall)
		}
	}
	if len(calls) != 2 || calls[0].ID != "a" || string(calls[0].Args) != `{"p":"x"}` || calls[1].ID != "b" || string(calls[1].Args) != "{}" {
		t.Fatalf("calls = %+v, want a then b with their own arguments", calls)
	}
}

func TestOpenAIStream_IdlessCallsAtOneIndexStaySeparate(t *testing.T) {
	got, err := openAIStream(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{}"}}]}}]}`+"\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"ls","arguments":""}}]}}]}`+"\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var calls []*agent.ToolCall
	for _, c := range got {
		if c.Kind == agent.StreamChunkToolCall {
			calls = append(calls, c.ToolCall)
		}
	}
	if len(calls) != 2 || calls[1].ID != "b" || string(calls[1].Args) != "{}" {
		t.Fatalf("calls = %+v, want two calls, the second with empty arguments defaulted to {}", calls)
	}
}

// PRV-5: a mid-stream error chunk (OpenRouter sends these on HTTP 200) is an
// error, not a finished reply.
func TestOpenAIStream_MidStreamErrorIsAnError(t *testing.T) {
	_, err := openAIStream(t,
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n",
		`data: {"error":{"code":502,"message":"upstream exploded"},"choices":[{"delta":{"content":""},"finish_reason":"error"}]}`+"\n\n",
	)
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("stream error = %v, want the upstream message", err)
	}
}

// PRV-5: a connection that closes before any finish or [DONE] is truncated.
func TestOpenAIStream_TruncatedWithoutFinishIsAnError(t *testing.T) {
	_, err := openAIStream(t, `data: {"choices":[{"delta":{"content":"half a sen"}}]}`+"\n\n")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want io.ErrUnexpectedEOF", err)
	}
}

// PRV-5: a stream that ends with [DONE] but no finish_reason is still whole.
func TestOpenAIStream_DoneWithoutFinishIsClean(t *testing.T) {
	_, err := openAIStream(t, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n", "data: [DONE]\n\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// PRV-5: Anthropic streams that stop before message_stop are truncated.
func TestAnthropicStream_TruncatedWithoutMessageStopIsAnError(t *testing.T) {
	srv := sseServer(t,
		"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half\"}}\n\n",
	)
	defer srv.Close()
	p := NewAnthropic("k", "claude-sonnet-5")
	p.BaseURL = srv.URL
	chunks, errs := p.StreamTurn(context.Background(), nil, nil, "")
	_, err := collectStream(t, chunks, errs)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want io.ErrUnexpectedEOF", err)
	}
}

// PRV-14: a server that goes silent mid-stream fails the turn instead of
// holding it open.
func TestOpenAIStream_StalledStreamIsAnError(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 50 * time.Millisecond
	defer func() { streamIdleTimeout = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	p := NewOpenAI("k", "gpt-4o")
	p.BaseURL = srv.URL
	chunks, errs := p.StreamTurn(context.Background(), nil, nil, "")
	_, err := collectStream(t, chunks, errs)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("stream error = %v, want a stall error", err)
	}
}

// PRV-14: transient overload from Anthropic (529) is retried before any
// body is streamed.
func TestDoWithRetry_RetriesOverloaded(t *testing.T) {
	old := rateLimitBackoff
	rateLimitBackoff = []time.Duration{time.Millisecond}
	defer func() { rateLimitBackoff = old }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(529)
		}
	}))
	defer srv.Close()

	resp, err := doWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil || resp.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("status=%v err=%v calls=%d, want 200 after one retry", resp, err, calls)
	}
}

// PRV-14: an error body that is not the usual JSON still says what happened.
func TestHTTPError_ReportsNonJSONAndStringErrors(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"html":   {"<html><body>Bad Gateway from proxy</body></html>", "Bad Gateway from proxy"},
		"string": {`{"error":"quota exceeded"}`, "quota exceeded"},
		"object": {`{"error":{"message":"bad key"}}`, "bad key"},
	}
	for name, c := range cases {
		resp := &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(c.body))}
		err := httpError(resp, "provider")
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "502") {
			t.Errorf("%s: error = %v, want 502 and %q", name, err, c.want)
		}
	}
}

// PRV-19: a consumer that stops reading must not strand the stream goroutine
// on a full chunk channel once the turn is cancelled.
func TestOpenAIStream_CancelReleasesBlockedSender(t *testing.T) {
	var events strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&events, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	srv := sseServer(t, events.String())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := NewOpenAI("k", "gpt-4o")
	p.BaseURL = srv.URL
	chunks, errs := p.StreamTurn(ctx, nil, nil, "")
	// Let the sender fill the buffer and block, then cancel without reading.
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Only the error channel is read: draining chunks would unblock a sender
	// that never saw the cancel, hiding the leak.
	done := make(chan struct{})
	go func() {
		for range errs {
		}
		close(done)
	}()
	_ = chunks
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream goroutine is still blocked after cancel")
	}
}
