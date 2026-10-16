package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// modelsClient bounds a /models listing. It runs on the request path, so a
// hung endpoint must not hold a turn, or a shutdown, open.
var modelsClient = &http.Client{Timeout: 15 * time.Second}

// openRouterContext remembers each OpenRouter model's context_length from the
// last catalog fetch (fetchModels), keyed by lowercased id: the agent asks
// for a model's window long after the client listed it.
var openRouterContext sync.Map

// OpenRouterContextLength is model's context_length from the latest
// OpenRouter catalog this process fetched, 0 if it hasn't fetched one or the
// model isn't in it.
func OpenRouterContextLength(model string) int {
	n, _ := openRouterContext.Load(strings.ToLower(model))
	v, _ := n.(int)
	return v
}

// ModelInfo is one model available through a provider credential.
type ModelInfo struct {
	ID          string
	DisplayName string
	// Capabilities mirrors the upstream's own shape rather than a unified
	// schema (OpenRouter's reasoning/pricing/context_length objects, a
	// hand-maintained table for OpenAI) — the three providers don't share
	// one, so forcing them into a common struct is where this kind of field
	// rots.
	Capabilities map[string]any
	// IsLegacy marks a model T3 still serves but no longer lists as current
	// (manifest status "legacy") — included rather than dropped so a client
	// can still target it, just grouped/sorted behind current models.
	IsLegacy bool
}

// openAIReasoningModels is a hand-maintained table of OpenAI model ID
// prefixes that accept the `reasoning_effort` request parameter — the
// OpenAI /v1/models endpoint doesn't report this itself, unlike
// OpenRouter's per-model "reasoning" object.
var openAIReasoningModels = []struct {
	prefix  string
	efforts []string
}{
	{"o1", []string{"low", "medium", "high"}},
	{"o3", []string{"low", "medium", "high"}},
	{"o4-mini", []string{"low", "medium", "high"}},
	{"gpt-5", []string{"minimal", "low", "medium", "high"}},
}

func openAICapabilities(id string) map[string]any {
	for _, m := range openAIReasoningModels {
		if strings.HasPrefix(id, m.prefix) {
			return map[string]any{"reasoningEfforts": m.efforts}
		}
	}
	return nil
}

// ListModels queries the models available through cfg's own credential
// (anthropic/openai/openrouter each expose a /models endpoint keyed by the
// same API key used for chat completions), so a client can offer every
// model one registered key unlocks instead of the user registering a
// separate named provider per model.
func ListModels(cfg ProviderConfig, manifest *ManifestCache) ([]ModelInfo, error) {
	models, err := listModels(cfg, manifest)
	if err != nil {
		return nil, err
	}
	switch cfg.Kind {
	case ProviderKindCLIClaude:
		return withPermissionModesBy(models, func(m ModelInfo) []ModeSpec {
			auto, _ := m.Capabilities["autoMode"].(bool)
			return claudeModes(auto)
		}), nil
	case ProviderKindCLICodex:
		return withPermissionModes(models, codexModes()), nil
	case ProviderKindAPIKey:
		return withPermissionModes(models, apiModes()), nil
	}
	switch kind, _ := CLIKindOf(cfg.Kind); kind {
	case CLIKindAmp:
		return withPermissionModes(models, ampModes()), nil
	case CLIKindPi, CLIKindOhMyPi:
		return withPermissionModes(models, piModes(piFlavorOf(kind))), nil
	}
	return models, nil // the ACP agents attach their own, from what they advertise
}

// ModelsFallback reports that ListModels for k answered from the built-in manifest because the
// agent itself couldn't be asked (logged out, failed). Only Claude and Codex are discovered this way.
func ModelsFallback(k ProviderKind) bool {
	kind, ok := CLIKindOf(k)
	return ok && (kind == CLIKindClaude || kind == CLIKindCodex) && discovered.degraded(kind)
}

// RetryModels lifts the backoff after a failed discovery so the next ListModels asks the agent again.
func RetryModels(k ProviderKind) {
	if kind, ok := CLIKindOf(k); ok {
		discovered.retry(kind)
	}
}

func listModels(cfg ProviderConfig, manifest *ManifestCache) ([]ModelInfo, error) {
	switch cfg.Kind {
	case ProviderKindCLIClaude:
		// Claude Code answers an initialize request with the list /model shows
		// for this account; the manifest (T3 Code's, see manifest.go) adds what
		// it doesn't report and stands in when it can't be asked (discover.go).
		home, _ := os.UserHomeDir()
		return ClaudeModelsDiscovered(manifest, home), nil
	case ProviderKindCLICodex:
		home, _ := os.UserHomeDir()
		return CodexModelsDiscovered(manifest, home), nil
	case ProviderKindAPIKey:
		// fall through
	default:
		// The ACP agents only say which models they offer inside a session.
		if kind, ok := CLIKindOf(cfg.Kind); ok {
			if kind == CLIKindAmp {
				return ampModels(), nil // its modes; it has no model inventory to ask
			}
			if kind == CLIKindPi || kind == CLIKindOhMyPi {
				home, _ := os.UserHomeDir()
				return PiModelsDiscovered(kind, home), nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			home, _ := os.UserHomeDir()
			return ListACPModels(ctx, kind, home)
		}
		return nil, fmt.Errorf("unknown provider kind %q", cfg.Kind)
	}

	switch cfg.HTTPProvider {
	case "anthropic":
		return anthropicModels(cfg)
	case "openai":
		return fetchModels(openAIModelsURL(cfg.BaseURL), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}, "", cfg.HTTPProvider)
	case "openrouter":
		// Public catalog, same for every key; the key isn't required but
		// doesn't hurt to send. A custom endpoint serves its own list, as chat does.
		listURL := "https://openrouter.ai/api/v1/models"
		if cfg.BaseURL != "" {
			listURL = openAIModelsURL(cfg.BaseURL)
		}
		return fetchModels(listURL, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}, "name", cfg.HTTPProvider)
	default:
		return nil, fmt.Errorf("unsupported httpProvider %q", cfg.HTTPProvider)
	}
}

// openAIModelsURL derives the /v1/models endpoint from a chat-completions
// BaseURL, so this works for the default OpenAI API and for OpenAI-compatible
// local servers alike. A bare host gets the same /v1 that chat gets.
func openAIModelsURL(baseURL string) string {
	if baseURL == "" {
		return "https://api.openai.com/v1/models"
	}
	root := strings.TrimSuffix(normalizeOpenAIBaseURL(baseURL), "/chat/completions")
	return root + "/models"
}

// anthropicModels lists /v1/models a page at a time: the API returns 20
// models unless asked for more, and says when more follow.
func anthropicModels(cfg ProviderConfig) ([]ModelInfo, error) {
	root := "https://api.anthropic.com"
	if cfg.BaseURL != "" {
		root = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1/messages")
	}
	setHeaders := func(req *http.Request) {
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	var all []ModelInfo
	after := ""
	for range 20 { // a cap: a provider that never says it's done can't loop us forever
		pageURL := root + "/v1/models?limit=1000"
		if after != "" {
			pageURL += "&after_id=" + url.QueryEscape(after)
		}
		models, hasMore, last, err := fetchModelPage(pageURL, setHeaders, "display_name", cfg.HTTPProvider)
		if err != nil {
			return nil, err
		}
		all = append(all, models...)
		if !hasMore || last == "" || last == after {
			break
		}
		after = last
	}
	return all, nil
}

func fetchModels(listURL string, setHeaders func(*http.Request), nameField string, httpProvider string) ([]ModelInfo, error) {
	models, _, _, err := fetchModelPage(listURL, setHeaders, nameField, httpProvider)
	return models, err
}

// fetchModelPage is one GET of a models listing, with the paging fields
// (has_more, last_id) when the provider sends them.
func fetchModelPage(listURL string, setHeaders func(*http.Request), nameField string, httpProvider string) (models []ModelInfo, hasMore bool, lastID string, err error) {
	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, false, "", err
	}
	setHeaders(req)

	resp, err := modelsClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = withoutQuery(ue.URL)
		}
		return nil, false, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, "", fmt.Errorf("listing models: HTTP %d from %s", resp.StatusCode, withoutQuery(listURL))
	}

	var body struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
		LastID  string           `json:"last_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, "", err
	}

	models = make([]ModelInfo, 0, len(body.Data))
	seen := make(map[string]bool, len(body.Data)) // dedupe case-insensitively, see casing-dupe note below
	for _, m := range body.Data {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		// OpenRouter marks a floating alias (e.g. "anthropic/claude-3.7-sonnet"
		// aliasing a pinned dated slug) with a populated alias_target — that's
		// the documented signal, not the "~" prefix convention used only when
		// *calling* chat completions with one.
		if aliasTarget, _ := m["alias_target"].(string); httpProvider == "openrouter" && aliasTarget != "" {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			// Genuine same-model casing dupes from one /models response, if
			// they occur, aren't a yoakeland-side cache bug — nothing here
			// caches the list between requests (every list_models call
			// re-fetches). Drop the repeat defensively either way.
			continue
		}
		seen[key] = true

		display, _ := m[nameField].(string)
		var caps map[string]any
		switch httpProvider {
		case "openrouter":
			caps = map[string]any{}
			for _, f := range []string{"context_length", "pricing", "top_provider", "reasoning", "architecture"} {
				if v, ok := m[f]; ok {
					caps[f] = v
				}
			}
			if n, ok := caps["context_length"].(float64); ok && n > 0 {
				openRouterContext.Store(key, int(n))
			}
			// architecture.input_modalities is the one place a provider actually
			// says which modalities a model takes; the vision gate reads it back
			// (SupportsImages) instead of guessing from the id. A response
			// without the member says nothing, so it isn't remembered.
			if arch, ok := caps["architecture"]; ok {
				rememberVision(key, takesImages(arch))
			}
			if len(caps) == 0 {
				caps = nil
			}
		case "openai":
			caps = openAICapabilities(id)
		}
		models = append(models, ModelInfo{ID: id, DisplayName: display, Capabilities: caps})
	}
	return models, body.HasMore, body.LastID, nil
}

// withoutQuery drops a URL's query string from an error message.
func withoutQuery(u string) string {
	before, _, _ := strings.Cut(u, "?")
	return before
}
