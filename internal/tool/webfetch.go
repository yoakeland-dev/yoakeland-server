package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
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

// blockedFetchRanges are IANA special-purpose ranges that net.IP's Is*
// methods don't cover — the set SSRF libraries such as code.dny.dev/ssrf
// block by default: CGNAT (where Tailscale lives, a realistic neighbour for
// this server), "this network" (0.x reaches localhost on Linux), IETF
// protocol assignments, benchmarking, the documentation nets, class E, and
// the IPv6 prefixes that embed an IPv4 address (IPv4-compatible, NAT64,
// 6to4, Teredo), which could otherwise smuggle 127.0.0.1 or 10.x past the
// checks.
var blockedFetchRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

func isBlockedFetchTarget(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return true // loopback, link-local, multicast, unspecified, RFC 1918, ULA
	}
	for _, p := range blockedFetchRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch a web page and return its main readable text (title and body), with navigation, ads, and scripts stripped out."
}

// RequiresPermission: file_read never asks, so an unasked fetch would let a
// prompt injection read .env and send it out in a URL. The permission is
// per host (see agent.permissionTarget), so "remember" covers a whole site.
func (t *WebFetchTool) RequiresPermission(_ []byte) bool { return true }

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
		return WrapUntrusted(args.URL, text), nil
	}
	return WrapUntrusted(args.URL, title+"\n\n"+text), nil
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
