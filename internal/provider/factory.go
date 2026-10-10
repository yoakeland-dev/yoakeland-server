package provider

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	ProviderName string // "anthropic", "openai", "openrouter", or "auto"
	Model        string
	APIKey       string
	BaseURL      string
	// NoEnv builds from the fields alone: no API key or base URL comes from the daemon's
	// environment and no backend is guessed from it. For credentials a client registered.
	NoEnv bool
}

func normalizeOpenAIBaseURL(raw string) string {
	raw = strings.TrimRight(raw, "/")
	if strings.HasSuffix(raw, "/chat/completions") {
		return raw
	}
	if strings.HasSuffix(raw, "/v1") {
		return raw + "/chat/completions"
	}
	return raw + "/v1/chat/completions"
}

func ResolveProvider(cfg Config) (Provider, error) {
	name := cfg.ProviderName
	getenv := os.Getenv
	if cfg.NoEnv {
		getenv = func(string) string { return "" }
	}

	// 1. If a custom BaseURL is passed (e.g. LM Studio, Ollama, vLLM), default to openai
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = getenv("OPENAI_BASE_URL")
	}

	if (name == "" || name == "auto") && baseURL != "" {
		name = "openai"
	}

	// 2. Auto-detect based on env
	if name == "" || name == "auto" {
		if getenv("ANTHROPIC_API_KEY") != "" {
			name = "anthropic"
		} else if getenv("OPENROUTER_API_KEY") != "" {
			name = "openrouter"
		} else if getenv("OPENAI_API_KEY") != "" {
			name = "openai"
		} else {
			return nil, fmt.Errorf("no provider specified. Set -provider, -base-url, or an API key (ANTHROPIC_API_KEY, OPENROUTER_API_KEY, OPENAI_API_KEY)")
		}
	}

	apiKey := cfg.APIKey

	switch name {
	case "anthropic":
		if apiKey == "" {
			apiKey = getenv("ANTHROPIC_API_KEY")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("missing ANTHROPIC_API_KEY")
		}
		p := NewAnthropic(apiKey, cfg.Model)
		if baseURL != "" {
			p.BaseURL = baseURL
		}
		return p, nil

	case "openai":
		if apiKey == "" {
			apiKey = getenv("OPENAI_API_KEY")
		}
		// When targeting local servers (localhost / 127.0.0.1 / LM Studio), allow a dummy key
		if apiKey == "" {
			if baseURL != "" && (strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "127.0.0.1")) {
				apiKey = "lm-studio"
			} else {
				return nil, fmt.Errorf("missing OPENAI_API_KEY")
			}
		}

		p := NewOpenAI(apiKey, cfg.Model)
		if baseURL != "" {
			p.BaseURL = normalizeOpenAIBaseURL(baseURL)
		}
		return p, nil

	case "openrouter":
		if apiKey == "" {
			apiKey = getenv("OPENROUTER_API_KEY")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("missing OPENROUTER_API_KEY")
		}
		p := NewOpenRouter(apiKey, cfg.Model)
		if baseURL != "" {
			p.BaseURL = normalizeOpenAIBaseURL(baseURL)
		}
		return p, nil

	default:
		return nil, fmt.Errorf("unsupported provider %q (supported: anthropic, openai, openrouter)", name)
	}
}
