package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/provider"
)

// drainStream collects every chunk from a StreamTurn call, failing the test
// on an unexpected stream error (pass wantErr=true to instead require one and
// return it).
func drainStream(t *testing.T, chunks <-chan agent.ModelStreamChunk, errs <-chan error, wantErr bool) ([]agent.ModelStreamChunk, error) {
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
		}
	}
	if wantErr && streamErr == nil {
		t.Fatal("expected a stream error, got none")
	}
	if !wantErr && streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	return got, streamErr
}

// TestAnthropicProvider_StreamTurn_ReportsMaxTokensAsLength guards the
// finding that a response truncated by hitting max_tokens (stop_reason
// "max_tokens" on message_delta, per Anthropic's Messages API) used to be
// indistinguishable from an ordinary clean finish: the turn loop only ever
// set finishReason to "stop" when a round-trip produced no tool calls,
// regardless of why the stream ended. Surfacing it as "length" lets a client
// tell a complete answer from one cut off mid-thought.
func TestAnthropicProvider_StreamTurn_ReportsMaxTokensAsLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"this got cut off\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":8192}}\n\n")
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", "claude-sonnet-5")
	p.BaseURL = srv.URL

	chunks, errs := p.StreamTurn(context.Background(), nil, nil)
	got, _ := drainStream(t, chunks, errs, false)

	var finishReason string
	for _, c := range got {
		if c.Kind == agent.StreamChunkFinish {
			finishReason = c.FinishReason
		}
	}
	if finishReason != "length" {
		t.Errorf("finish reason = %q, want %q", finishReason, "length")
	}
}

// TestAnthropicProvider_StreamTurn_SurfacesMidStreamError guards the finding
// that Anthropic's `event: error` SSE event (e.g. overloaded_error on a long,
// busy request) fell through the content_block_start/delta/stop switch
// unhandled: the stream simply ended with no more events, which the reader
// couldn't tell apart from a normal finish, so the failure was silently
// reported as a completed turn.
func TestAnthropicProvider_StreamTurn_SurfacesMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", "claude-sonnet-5")
	p.BaseURL = srv.URL

	chunks, errs := p.StreamTurn(context.Background(), nil, nil)
	_, streamErr := drainStream(t, chunks, errs, true)

	if !strings.Contains(streamErr.Error(), "overloaded_error") {
		t.Errorf("stream error = %v, want it to mention overloaded_error", streamErr)
	}
}

// TestOpenAICompatibleProvider_StreamTurn_ReportsLengthFinish is the
// OpenAI/OpenRouter analog: finish_reason "length" means the response was
// truncated at the token limit, and was previously dropped on the floor the
// same way as the Anthropic case above.
func TestOpenAICompatibleProvider_StreamTurn_ReportsLengthFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"this got cut off\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "gpt-4o")
	p.BaseURL = srv.URL

	chunks, errs := p.StreamTurn(context.Background(), nil, nil)
	got, _ := drainStream(t, chunks, errs, false)

	var finishReason string
	for _, c := range got {
		if c.Kind == agent.StreamChunkFinish {
			finishReason = c.FinishReason
		}
	}
	if finishReason != "length" {
		t.Errorf("finish reason = %q, want %q", finishReason, "length")
	}
}
