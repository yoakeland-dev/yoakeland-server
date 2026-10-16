package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

type WebSearchArgs struct {
	Query string `json:"query"`
}

// WebSearchTool searches the web. With a Brave API key it queries Brave's
// JSON API, which is the reliable path since it returns structured results
// instead of HTML that breaks whenever page markup changes. With no key it
// falls back to scraping DuckDuckGo's HTML results — free and keyless, but
// DuckDuckGo actively bot-checks non-browser/datacenter traffic, so this
// path can intermittently return a "blocked" error instead of results.
// Set BRAVE_API_KEY for the reliable path.
type WebSearchTool struct {
	apiKey     string
	client     *http.Client
	baseURL    string // Brave endpoint, overridden in tests
	ddgBaseURL string // DuckDuckGo endpoint, overridden in tests
}

func NewWebSearchTool(apiKey string) *WebSearchTool {
	return &WebSearchTool{
		apiKey:     apiKey,
		client:     &http.Client{Timeout: 15 * time.Second},
		baseURL:    "https://api.search.brave.com/res/v1/web/search",
		ddgBaseURL: "https://duckduckgo.com/html/",
	}
}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) Description() string {
	return "Search the web and return matching page titles, URLs, and snippets."
}

// MaxWebSearchQueryRunes caps a query: the query is sent to a third party, so
// a long one is mostly a prompt-injection payload or leaked file contents.
const MaxWebSearchQueryRunes = 256

// RequiresPermission: a query leaves the machine, so it asks like web_fetch
// does. The permission is for the query as a whole (see agent.permissionTarget).
func (t *WebSearchTool) RequiresPermission(_ []byte) bool {
	return true
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
	if utf8.RuneCountInString(args.Query) > MaxWebSearchQueryRunes {
		return "", fmt.Errorf("query must be at most %d characters", MaxWebSearchQueryRunes)
	}

	var results string
	var err error
	if t.apiKey == "" {
		results, err = t.executeDDG(ctx, args.Query)
	} else {
		results, err = t.executeBrave(ctx, args.Query)
	}
	if err != nil {
		return "", err
	}
	return WrapUntrusted("web search results", results), nil
}

func (t *WebSearchTool) executeBrave(ctx context.Context, query string) (string, error) {
	reqURL := t.baseURL + "?q=" + url.QueryEscape(query)
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

func (t *WebSearchTool) executeDDG(ctx context.Context, query string) (string, error) {
	reqURL := t.ddgBaseURL + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; yoakeland-web-search/1.0)")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search request failed: status %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("parsing search response: %w", err)
	}

	if doc.Find(".anomaly-modal").Length() > 0 {
		return "", fmt.Errorf("DuckDuckGo blocked this request as a bot; set BRAVE_API_KEY for reliable search")
	}

	var b strings.Builder
	n := 0
	doc.Find(".results .web-result").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if n >= 10 {
			return false
		}
		title := strings.TrimSpace(s.Find(".result__a").Text())
		link := resolveDDGRedirect(strings.TrimSpace(s.Find(".result__a").AttrOr("href", "")))
		snippet := strings.TrimSpace(s.Find(".result__snippet").Text())
		if title == "" || link == "" {
			return true
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", n, title, link, snippet)
		return true
	})

	if n == 0 {
		return "No results found.", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// resolveDDGRedirect unwraps DuckDuckGo's outbound-click redirect
// (//duckduckgo.com/l/?uddg=<encoded-target>&...) to the real target URL.
func resolveDDGRedirect(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
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
