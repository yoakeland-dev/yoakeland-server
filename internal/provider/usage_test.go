package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/provider"
)

// TestAnthropicProvider_StreamTurn_EmitsUsage guards against the usage
// tracking regressing to always-zero: a compromised/expired fix here means
// -max-session-cost-usd can never trip for real Anthropic API-key sessions.
func TestAnthropicProvider_StreamTurn_EmitsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"usage\":{\"output_tokens\":42}}\n\n")
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", "claude-3-5-sonnet-20241022")
	p.BaseURL = srv.URL

	chunks, errs := p.StreamTurn(context.Background(), nil, nil)

	var gotUsage *agent.ModelStreamChunk
	for chunks != nil || errs != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			if c.Kind == agent.StreamChunkUsage {
				cc := c
				gotUsage = &cc
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("unexpected stream error: %v", err)
			}
		}
	}

	if gotUsage == nil || gotUsage.Usage == nil {
		t.Fatal("expected a StreamChunkUsage chunk, got none")
	}
	if gotUsage.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", gotUsage.Usage.InputTokens)
	}
	if gotUsage.Usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42 (message_delta should override message_start's initial count)", gotUsage.Usage.OutputTokens)
	}
	if gotUsage.Usage.TotalTokens != 142 {
		t.Errorf("TotalTokens = %d, want 142", gotUsage.Usage.TotalTokens)
	}
}

// TestOpenAICompatibleProvider_StreamTurn_EmitsUsage is the OpenAI/OpenRouter
// analog of the Anthropic test above — same regression it guards against.
func TestOpenAICompatibleProvider_StreamTurn_EmitsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "gpt-4o")
	p.BaseURL = srv.URL

	chunks, errs := p.StreamTurn(context.Background(), nil, nil)

	var gotUsage *agent.ModelStreamChunk
	for chunks != nil || errs != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			if c.Kind == agent.StreamChunkUsage {
				cc := c
				gotUsage = &cc
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("unexpected stream error: %v", err)
			}
		}
	}

	if gotUsage == nil || gotUsage.Usage == nil {
		t.Fatal("expected a StreamChunkUsage chunk, got none")
	}
	if gotUsage.Usage.InputTokens != 10 || gotUsage.Usage.OutputTokens != 5 || gotUsage.Usage.TotalTokens != 15 {
		t.Errorf("got usage %+v, want {InputTokens:10 OutputTokens:5 TotalTokens:15 ...}", gotUsage.Usage)
	}
}
