package provider

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewManifestCacheFallsBackToEmbedded(t *testing.T) {
	mc := NewManifestCache("")
	claude := mc.ClaudeModels()
	if len(claude) == 0 {
		t.Fatalf("expected embedded fallback to seed at least one claude model")
	}
	found := false
	for _, m := range claude {
		if m.ID == "claude-sonnet-5" {
			found = true
			if m.DisplayName != "Claude Sonnet 5" {
				t.Errorf("DisplayName = %q, want %q", m.DisplayName, "Claude Sonnet 5")
			}
			if m.Capabilities["reasoningEfforts"] == nil {
				t.Errorf("expected reasoningEfforts capability cross-walked from optionDescriptors")
			}
		}
	}
	if !found {
		t.Fatalf("expected claude-sonnet-5 in embedded fallback, got %+v", claude)
	}

	codex := mc.CodexModels()
	if len(codex) == 0 {
		t.Fatalf("expected embedded fallback to seed at least one codex model")
	}
}

func TestOptionDescriptorCapabilitiesFiltersPromptInjectedValues(t *testing.T) {
	caps := optionDescriptorCapabilities([]manifestOptionDescriptor{
		{
			ID: "effort",
			Options: []struct {
				ID string `json:"id"`
			}{{ID: "low"}, {ID: "high"}, {ID: "ultrathink"}},
			PromptInjectedValues: []string{"ultrathink"},
		},
		{ID: "fastMode"},
		{
			ID: "contextWindow",
			Options: []struct {
				ID string `json:"id"`
			}{{ID: "200k"}, {ID: "1m"}},
		},
	})

	efforts, _ := caps["reasoningEfforts"].([]string)
	if len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "high" {
		t.Errorf("reasoningEfforts = %v, want [low high] (ultrathink filtered out)", efforts)
	}
	if caps["fastMode"] != true {
		t.Errorf("fastMode = %v, want true", caps["fastMode"])
	}
	windows, _ := caps["contextWindows"].([]string)
	if len(windows) != 2 {
		t.Errorf("contextWindows = %v, want [200k 1m]", windows)
	}
}

func TestManifestCacheRefreshConditionalGET(t *testing.T) {
	const body = `{"currentModels":{"claudeAgent":["claude-x"],"codex":["gpt-x"]},"providers":{"claudeAgent":{"profiles":{},"models":[{"slug":"claude-x","name":"Claude X"}]}}}`
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	mc := NewManifestCache(dir)
	mc.httpClient = srv.Client()
	mc.url = srv.URL

	if err := mc.Refresh(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	models := mc.ClaudeModels()
	if len(models) != 1 || models[0].ID != "claude-x" || models[0].DisplayName != "Claude X" {
		t.Fatalf("unexpected models after refresh: %+v", models)
	}
	if _, err := os.Stat(filepath.Join(dir, "model-manifest-cache.json")); err != nil {
		t.Errorf("expected on-disk cache file to be written: %v", err)
	}

	if err := mc.Refresh(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}

	// Reload from disk to confirm the cache round-trips.
	mc2 := NewManifestCache(dir)
	if got := mc2.ClaudeModels(); len(got) != 1 || got[0].ID != "claude-x" {
		t.Fatalf("reloaded cache = %+v, want claude-x", got)
	}
}

func TestManifestCacheKeepsLastGoodOnMalformedFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"totally": "reshaped"}`))
	}))
	defer srv.Close()

	mc := NewManifestCache("")
	mc.url = srv.URL
	before := mc.ClaudeModels()
	if err := mc.Refresh(); err == nil {
		t.Fatalf("expected malformed manifest to return an error")
	}
	after := mc.ClaudeModels()
	if len(before) != len(after) {
		t.Fatalf("expected last-good cache to survive a malformed fetch: before=%d after=%d", len(before), len(after))
	}
}
