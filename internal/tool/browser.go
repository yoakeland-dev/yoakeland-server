package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// BrowserRelay performs one browser action in sessionID's browser panel on
// whichever client hosts it (see cmd/yoakeland's browserBroker), returning
// the text the agent gets back.
type BrowserRelay interface {
	Browse(ctx context.Context, sessionID, action string, args json.RawMessage) (string, error)
}

var browserActions = []string{"navigate", "snapshot", "click", "type", "press", "scroll", "evaluate", "back", "forward", "reload"}

// BrowserTool lets the agent use the browser panel beside the chat — the
// same one the user sees, so they watch it work. Text-first like Playwright
// MCP: a snapshot lists the page's visible text and interactive elements,
// each with a ref, and actions target a ref rather than pixel coordinates.
type BrowserTool struct{ relay BrowserRelay }

func NewBrowserTool(relay BrowserRelay) *BrowserTool { return &BrowserTool{relay} }

func (t *BrowserTool) Name() string { return "browser" }
func (t *BrowserTool) Description() string {
	return "Use the browser panel beside the chat (the user sees what you do). Workflow: navigate to a URL, read the " +
		"returned snapshot (page text plus interactive elements, each tagged [ref=N]), then click/type/press using those refs. " +
		"Every action returns a fresh snapshot; refs are only valid for the latest one. For a dev server you started with " +
		"terminal_start, navigate to http://localhost:<port>. evaluate runs a JavaScript expression in the page and returns " +
		"its JSON result. Console errors from the page are included in snapshots."
}

// RequiresPermission: evaluate runs arbitrary script in whatever page is
// open, and navigate can take the agent to any site the user is logged in
// to (or carry data out in the URL) — both ask, except navigating to the
// local dev servers this tool is mostly for.
// ponytail: a click can still follow a link off-site once a page is open;
// tracking the page's origin needs the browser host to report it.
func (t *BrowserTool) RequiresPermission(argsJSON []byte) bool {
	var a browserArgs
	_ = json.Unmarshal(argsJSON, &a)
	return a.Action == "evaluate" || (a.Action == "navigate" && !isLoopbackURL(a.URL))
}

// browserArgs is both what the permission check reads and what the browser
// host receives (re-marshaled), so the two can't disagree: Go matches JSON
// keys case-insensitively, a JS client doesn't, and forwarding the raw args
// would let {"url":evil,"Url":localhost} pass as localhost here.
type browserArgs struct {
	Action     string `json:"action"`
	URL        string `json:"url,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Text       string `json:"text,omitempty"`
	Submit     bool   `json:"submit,omitempty"`
	Key        string `json:"key,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Expression string `json:"expression,omitempty"`
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func (t *BrowserTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: t.Description(), Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":     map[string]any{"type": "string", "enum": browserActions},
			"url":        map[string]any{"type": "string", "description": "navigate: the URL (http/https)."},
			"ref":        map[string]any{"type": "string", "description": "click/type/scroll: an element's ref from the latest snapshot."},
			"text":       map[string]any{"type": "string", "description": "type: text to enter; replaces the field's current value."},
			"submit":     map[string]any{"type": "boolean", "description": "type: press Enter afterwards."},
			"key":        map[string]any{"type": "string", "description": "press: a key such as Enter, Escape, Tab, ArrowDown, or a character."},
			"direction":  map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "scroll: page direction when no ref is given."},
			"expression": map[string]any{"type": "string", "description": "evaluate: a JavaScript expression; its value must be JSON-serializable."},
		},
		"required": []string{"action"},
	}}
}

func (t *BrowserTool) Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var a browserArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	known := false
	for _, v := range browserActions {
		known = known || v == a.Action
	}
	switch {
	case !known:
		return "", fmt.Errorf("unknown action %q", a.Action)
	case a.Action == "navigate" && a.URL == "":
		return "", fmt.Errorf("navigate needs a url")
	case a.Action == "navigate" && !isHTTPURL(a.URL):
		return "", fmt.Errorf("navigate only opens http(s) URLs")
	case (a.Action == "click" || a.Action == "type") && a.Ref == "":
		return "", fmt.Errorf("%s needs a ref from the latest snapshot", a.Action)
	case a.Action == "press" && a.Key == "":
		return "", fmt.Errorf("press needs a key")
	case a.Action == "evaluate" && a.Expression == "":
		return "", fmt.Errorf("evaluate needs an expression")
	}
	if t.relay == nil {
		return "", fmt.Errorf("no browser is available")
	}
	forwarded, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	out, err := t.relay.Browse(ctx, execCtx.SessionID, a.Action, forwarded)
	if err != nil {
		// The host's error can quote page-controlled text (a script exception, say).
		return "", errors.New(WrapUntrusted("the browser page", err.Error()))
	}
	return WrapUntrusted("the browser page", out), nil
}
