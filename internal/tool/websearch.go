package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type WebSearchArgs struct {
	Query string `json:"query"`
}

// WebSearchTool queries the Brave Search API. Brave over a scraped
// DuckDuckGo page because it returns structured JSON instead of HTML that
// breaks whenever the page markup changes.
type WebSearchTool struct {
	apiKey  string
	client  *http.Client
	baseURL string // overridden in tests to point at a fake server
}

func NewWebSearchTool(apiKey string) *WebSearchTool {
	return &WebSearchTool{
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 15 * time.Second},
		baseURL: "https://api.search.brave.com/res/v1/web/search",
	}
}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) Description() string {
	return "Search the web and return matching page titles, URLs, and snippets."
}

func (t *WebSearchTool) RequiresPermission(_ []byte) bool {
	return false
}

type braveSearchResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (t *WebSearchTool) Execute(ctx context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args WebSearchArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	reqURL := t.baseURL + "?q=" + url.QueryEscape(args.Query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", t.apiKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search request failed: status %d", resp.StatusCode)
	}

	var parsed braveSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decoding search response: %w", err)
	}

	if len(parsed.Web.Results) == 0 {
		return "No results found.", nil
	}

	var b strings.Builder
	for i, r := range parsed.Web.Results {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, r.Description)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (t *WebSearchTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
			},
			"required": []string{"query"},
		},
	}
}
