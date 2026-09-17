package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchModels_OpenRouterFiltersAliasesAndDedupes covers items 1 and 2 of
// the OpenRouter checklist: a model carrying a populated alias_target is a
// floating alias for another entry and must not reach the client, and two
// entries whose ids differ only by case collapse to one (whether or not
// OpenRouter genuinely does this, dropping the repeat is harmless).
func TestFetchModels_OpenRouterFiltersAliasesAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "anthropic/claude-3.7-sonnet", "name": "Claude 3.7 (floating)", "alias_target": "anthropic/claude-3-7-sonnet"},
				{"id": "anthropic/claude-3-7-sonnet", "name": "Claude 3.7 Sonnet", "context_length": 200000},
				{"id": "Anthropic/Claude-3-7-Sonnet", "name": "duplicate casing"},
				{"id": "openai/gpt-4o", "name": "GPT-4o", "pricing": map[string]any{"prompt": "0.000005"}},
			},
		})
	}))
	defer srv.Close()

	models, err := fetchModels(srv.URL, func(*http.Request) {}, "name", "openrouter")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected the alias and the casing dupe to be dropped, got %d models: %+v", len(models), models)
	}
	for _, m := range models {
		if m.ID == "anthropic/claude-3.7-sonnet" {
			t.Errorf("alias_target entry %q should have been filtered out", m.ID)
		}
	}

	byID := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}
	sonnet, ok := byID["anthropic/claude-3-7-sonnet"]
	if !ok {
		t.Fatal("expected the canonical (non-alias) sonnet entry to survive")
	}
	if sonnet.Capabilities["context_length"] == nil {
		t.Error("expected context_length to be passed through into Capabilities")
	}
}

func TestFetchModels_OpenAIReasoningEffortTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "o4-mini"},
				{"id": "gpt-4o"},
			},
		})
	}))
	defer srv.Close()

	models, err := fetchModels(srv.URL, func(*http.Request) {}, "", "openai")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	byID := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	if caps := byID["o4-mini"].Capabilities; caps == nil || caps["reasoningEfforts"] == nil {
		t.Errorf("expected o4-mini to carry a hardcoded reasoningEfforts capability, got %+v", byID["o4-mini"])
	}
	if caps := byID["gpt-4o"].Capabilities; caps != nil {
		t.Errorf("expected gpt-4o (non-reasoning) to carry no capabilities, got %+v", caps)
	}
}
