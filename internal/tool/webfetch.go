package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"codeberg.org/readeck/go-readability"
)

const (
	webFetchMaxBodyBytes   = 5 << 20 // don't buffer an unbounded response into memory
	webFetchMaxOutputChars = 8000    // keep one fetch from blowing the context budget
)

type WebFetchArgs struct {
	URL string `json:"url"`
}

// WebFetchTool downloads a page and extracts its main readable text (via
// go-readability, a Go port of Mozilla's Readability.js) so the model gets
// article content instead of nav/ads/script markup. The dial hook refuses
// loopback/private/link-local targets so a URL surfaced from search
// results or page content can't be used to reach internal services (SSRF),
// including via a redirect — Control runs per-connection on the resolved
// address, so it re-checks every hop.
type WebFetchTool struct {
	client *http.Client
}

func NewWebFetchTool() *WebFetchTool {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlockedFetchTarget(ip) {
				return fmt.Errorf("refusing to connect to internal/private address %s", host)
			}
			return nil
		},
	}
	return &WebFetchTool{
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{DialContext: dialer.DialContext},
		},
	}
}

func isBlockedFetchTarget(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch a web page and return its main readable text (title and body), with navigation, ads, and scripts stripped out."
}

func (t *WebFetchTool) RequiresPermission(_ []byte) bool { return false }

func (t *WebFetchTool) Execute(ctx context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args WebFetchArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		return "", fmt.Errorf("url must not be empty")
	}

	parsed, err := url.ParseRequestURI(args.URL)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported url scheme %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; yoakeland-web-fetch/1.0)")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch failed: status %d", resp.StatusCode)
	}

	article, err := readability.FromReader(io.LimitReader(resp.Body, webFetchMaxBodyBytes), parsed)
	if err != nil {
		return "", fmt.Errorf("extracting content: %w", err)
	}

	text := strings.TrimSpace(article.TextContent)
	if text == "" {
		return "", fmt.Errorf("no readable content found on page")
	}
	if len(text) > webFetchMaxOutputChars {
		text = text[:webFetchMaxOutputChars] + "\n\n[truncated]"
	}

	title := strings.TrimSpace(article.Title)
	if title == "" {
		return text, nil
	}
	return title + "\n\n" + text, nil
}

func (t *WebFetchTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to fetch.",
				},
			},
			"required": []string{"url"},
		},
	}
}
