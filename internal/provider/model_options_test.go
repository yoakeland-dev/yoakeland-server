package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/provider"
)

// TestAnthropicProvider_SetModelOptions_SetsThinkingBudget covers item 4 of
// the OpenRouter checklist: set_model_options must actually change the
// outgoing request, not just be accepted and dropped.
func TestAnthropicProvider_SetModelOptions_SetsThinkingBudget(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", "claude-3-5-sonnet-20241022")
	p.BaseURL = srv.URL
	p.SetModelOptions(agent.ModelOptions{ThinkingBudget: 4096})

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	for range cChan {
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	thinking, _ := captured["thinking"].(map[string]any)
	if thinking == nil {
		t.Fatalf("expected a thinking block in the request, got: %+v", captured)
	}
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(4096) {
		t.Errorf("unexpected thinking block: %+v", thinking)
	}
}

// TestOpenAICompatibleProvider_SetModelOptions_SetsReasoningEffort covers
// the OpenAI/OpenRouter half of the same checklist item.
func TestOpenAICompatibleProvider_SetModelOptions_SetsReasoningEffort(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "o4-mini")
	p.BaseURL = srv.URL
	p.SetModelOptions(agent.ModelOptions{ReasoningEffort: "high"})

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	for range cChan {
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if captured["reasoning_effort"] != "high" {
		t.Errorf("expected reasoning_effort=high in the request, got: %+v", captured)
	}
}

// TestOpenAICompatibleProvider_DefaultsMaxTokens guards the fix for
// premature, empty cutoffs on reasoning models: several OpenAI-compatible
// backends (DeepSeek's API included) apply their own low default (~4096)
// for max_tokens when it's omitted, which a reasoning model can burn
// entirely on hidden reasoning_content before writing any visible answer.
func TestOpenAICompatibleProvider_DefaultsMaxTokens(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "deepseek-reasoner")
	p.BaseURL = srv.URL

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	for range cChan {
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if captured["max_tokens"] != float64(8192) {
		t.Errorf("expected a generous default max_tokens, got: %+v", captured["max_tokens"])
	}
}

// TestOpenAICompatibleProvider_OmitsMaxTokensForRealOpenAI guards the other
// side of that default: genuine api.openai.com must NOT get it, since its
// o1/o3/o4/gpt-5 reasoning-family models reject max_tokens outright (they
// require max_completion_tokens instead) — sending it there would break
// exactly the models this default exists to help elsewhere.
func TestOpenAICompatibleProvider_OmitsMaxTokensForRealOpenAI(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "o4-mini")
	p.BaseURL = "https://api.openai.com/v1/chat/completions" // overridden below to actually hit the test server
	p.Client = srv.Client()
	// Route the request to the test server while keeping BaseURL literally
	// equal to the real OpenAI endpoint, which is what the provider branches
	// on — a RoundTripper swap is the only way to do that.
	p.Client.Transport = redirectTransport{to: srv.URL}

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	for range cChan {
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if _, ok := captured["max_tokens"]; ok {
		t.Errorf("expected no max_tokens for api.openai.com, got: %+v", captured["max_tokens"])
	}
}

// redirectTransport rewrites every request to hit `to` instead of its
// original URL, so a provider can be tested with its real BaseURL intact
// (for code that branches on it) while the request still lands on a local
// test server.
type redirectTransport struct{ to string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := url.Parse(t.to)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// TestAnthropicProvider_CapturesThinkingDelta guards reasoning capture: a
// thinking_delta content block must surface as its own StreamChunkReasoning
// chunk, not get silently dropped or mixed into the visible answer.
func TestAnthropicProvider_CapturesThinkingDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("" +
			"event: content_block_start\ndata: {\"content_block\":{\"type\":\"thinking\"}}\n\n" +
			"event: content_block_delta\ndata: {\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"let me think\"}}\n\n" +
			"event: content_block_stop\ndata: {}\n\n" +
			"event: content_block_start\ndata: {\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"the answer\"}}\n\n" +
			"event: content_block_stop\ndata: {}\n\n" +
			"event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", "claude-3-5-sonnet-20241022")
	p.BaseURL = srv.URL

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	var reasoning, text string
	for c := range cChan {
		switch c.Kind {
		case agent.StreamChunkReasoning:
			reasoning += c.Delta
		case agent.StreamChunkDelta:
			text += c.Delta
		}
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if reasoning != "let me think" {
		t.Errorf("reasoning = %q, want %q", reasoning, "let me think")
	}
	if text != "the answer" {
		t.Errorf("text = %q, want %q", text, "the answer")
	}
}

// TestOpenAICompatibleProvider_CapturesReasoningContent covers the same for
// an OpenAI-compatible provider's reasoning_content delta field
// (DeepSeek-reasoner and similar).
func TestOpenAICompatibleProvider_CapturesReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("" +
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"the answer\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := provider.NewOpenAI("test-key", "deepseek-reasoner")
	p.BaseURL = srv.URL

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil, "")
	var reasoning, text string
	for c := range cChan {
		switch c.Kind {
		case agent.StreamChunkReasoning:
			reasoning += c.Delta
		case agent.StreamChunkDelta:
			text += c.Delta
		}
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if reasoning != "thinking..." {
		t.Errorf("reasoning = %q, want %q", reasoning, "thinking...")
	}
	if text != "the answer" {
		t.Errorf("text = %q, want %q", text, "the answer")
	}
}
