package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

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
		return fetchModels("https://api.anthropic.com/v1/models", func(req *http.Request) {
			req.Header.Set("x-api-key", cfg.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}, "display_name", cfg.HTTPProvider)
	case "openai":
		return fetchModels(openAIModelsURL(cfg.BaseURL), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}, "", cfg.HTTPProvider)
	case "openrouter":
		// Public catalog, same for every key; the key isn't required but
		// doesn't hurt to send.
		return fetchModels("https://openrouter.ai/api/v1/models", func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}, "name", cfg.HTTPProvider)
	default:
		return nil, fmt.Errorf("unsupported httpProvider %q", cfg.HTTPProvider)
	}
}

// openAIModelsURL derives the /v1/models endpoint from a chat-completions
// BaseURL (as stored/normalized for OpenAICompatibleProvider), so this works
// for the default OpenAI API and for OpenAI-compatible local servers alike.
func openAIModelsURL(baseURL string) string {
	if baseURL == "" {
		return "https://api.openai.com/v1/models"
	}
	root := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/chat/completions")
	return root + "/models"
}

func fetchModels(url string, setHeaders func(*http.Request), nameField string, httpProvider string) ([]ModelInfo, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing models (%d): %s", resp.StatusCode, url)
	}

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(body.Data))
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
			for _, f := range []string{"context_length", "pricing", "top_provider", "reasoning"} {
				if v, ok := m[f]; ok {
					caps[f] = v
				}
			}
			if n, ok := caps["context_length"].(float64); ok && n > 0 {
				openRouterContext.Store(key, int(n))
			}
			if len(caps) == 0 {
				caps = nil
			}
		case "openai":
			caps = openAICapabilities(id)
		}
		models = append(models, ModelInfo{ID: id, DisplayName: display, Capabilities: caps})
	}
	return models, nil
}
