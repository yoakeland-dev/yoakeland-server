package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil)
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

	cChan, eChan := p.StreamTurn(context.Background(), nil, nil)
	for range cChan {
	}
	if err := <-eChan; err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}

	if captured["reasoning_effort"] != "high" {
		t.Errorf("expected reasoning_effort=high in the request, got: %+v", captured)
	}
}
