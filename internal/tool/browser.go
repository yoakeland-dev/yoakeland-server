package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"yoakeland/internal/protocol"
)

// BrowserResult is one browser action's outcome: the text the agent gets back,
// plus the capture a `screenshot` action produced.
type BrowserResult struct {
	Output string
	Image  *protocol.ImageData
}

// BrowserRelay performs one browser action in sessionID's browser panel on
// whichever client hosts it (see cmd/yoakeland's browserBroker).
type BrowserRelay interface {
	Browse(ctx context.Context, sessionID, action string, args json.RawMessage) (BrowserResult, error)
}

// maxBrowserImageBytes caps a forwarded screenshot. Providers take images
// this size (Anthropic stops at 5 MB, OpenAI at 20 MB), and every capture
// lands in the session file and in each request that follows, so a client
// that ignores the downscale guidance in SPEC.md §4e fails here rather than
// burning context.
const maxBrowserImageBytes = 4 << 20

var browserActions = []string{"navigate", "snapshot", "click", "type", "press", "scroll", "evaluate", "back", "forward", "reload", "hover", "select", "wait", "tabs", "screenshot"}

// browserActionsNoScreenshot is browserActions minus the one action whose
// result is a picture, for a model that can't see one (DefinitionWithoutImages
// below). screenshot is last in browserActions — keep it there.
var browserActionsNoScreenshot = browserActions[:len(browserActions)-1]

// BrowserTool lets the agent use the browser panel beside the chat — the
// same one the user sees, so they watch it work. Text-first like Playwright
// MCP: a snapshot lists the page's visible text and interactive elements,
// each with a ref, and actions target a ref rather than pixel coordinates.
type BrowserTool struct{ relay BrowserRelay }

func NewBrowserTool(relay BrowserRelay) *BrowserTool { return &BrowserTool{relay} }

func (t *BrowserTool) Name() string { return "browser" }

// browserDescription is everything about the tool that's true for any model,
// up to the screenshot action.
const browserDescription = "Use the browser panel beside the chat (the user sees what you do). Workflow: navigate to a URL, read the " +
	"returned snapshot (page text plus interactive elements, each tagged [ref=N]), then click/type/press using those refs. " +
	"Every action returns a fresh snapshot; refs are only valid for the latest one. For a dev server you started with " +
	"terminal_start, navigate to http://localhost:<port>. evaluate runs a JavaScript expression in the page and returns " +
	"its JSON result. hover moves the pointer over a ref (menus, tooltips). select picks an <option> of a dropdown ref by its " +
	"label or value (text). wait pauses for ms and/or until text shows up on the page, then returns a snapshot — use it " +
	"after an action that loads content. tabs works the panel's tabs: op list, new (blank; navigate afterwards), select or " +
	"close a tab by its number (as list numbers them, from 1); actions apply to the selected tab. Console errors from the " +
	"page are included in snapshots."

// browserScreenshotDescription is the rest of it, appended only for a model
// that can see the picture it would get back (DefinitionWithoutImages).
const browserScreenshotDescription = " screenshot captures the panel as a picture " +
	"you can see — reach for it when text isn't enough (a layout, a chart, a canvas, why something looks wrong); it's " +
	"slower and costs far more tokens than a snapshot, so use snapshot for structure and screenshot to actually look."

func (t *BrowserTool) Description() string {
	return browserDescription + browserScreenshotDescription
}

// RequiresPermission: evaluate runs arbitrary script in whatever page is
// open, and navigate can take the agent to any site the user is logged in
// to (or carry data out in the URL) — both ask, except navigating to the
// local dev servers this tool is mostly for. snapshot and screenshot don't
// ask: they only read the page that's already open, and the user is watching
// the panel they're taken from.
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
	Ms         int    `json:"ms,omitempty"`
	Op         string `json:"op,omitempty"`
	Tab        int    `json:"tab,omitempty"`
}

// maxBrowserWaitMs keeps one wait inside the broker's browserTimeout (60s) with room for the snapshot.
const maxBrowserWaitMs = 30_000

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
	return t.definition(browserActions, t.Description())
}

// DefinitionWithoutImages implements ImageTool: the same tool with the
// screenshot action left out of both the description and the schema, for a
// model that can't see the picture it would get back (SPEC.md §4e). Nothing
// else is hidden — the model still reads every page as text.
func (t *BrowserTool) DefinitionWithoutImages() ToolDefinition {
	return t.definition(browserActionsNoScreenshot, browserDescription)
}

func (t *BrowserTool) definition(actions []string, description string) ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: description, Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":     map[string]any{"type": "string", "enum": actions},
			"url":        map[string]any{"type": "string", "description": "navigate: the URL (http/https)."},
			"ref":        map[string]any{"type": "string", "description": "click/type/scroll/hover/select: an element's ref from the latest snapshot."},
			"text":       map[string]any{"type": "string", "description": "type: text to enter; replaces the field's current value. select: the option's label or value. wait: text to wait for."},
			"submit":     map[string]any{"type": "boolean", "description": "type: press Enter afterwards."},
			"key":        map[string]any{"type": "string", "description": "press: a key such as Enter, Escape, Tab, ArrowDown, or a character."},
			"direction":  map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "scroll: page direction when no ref is given."},
			"expression": map[string]any{"type": "string", "description": "evaluate: a JavaScript expression; its value must be JSON-serializable."},
			"ms":         map[string]any{"type": "integer", "description": "wait: milliseconds to pause (at most 30000); with text, how long to wait for it (default 10000)."},
			"op":         map[string]any{"type": "string", "enum": []string{"list", "new", "select", "close"}, "description": "tabs: what to do."},
			"tab":        map[string]any{"type": "integer", "description": "tabs select/close: the tab's number, from 1, as tabs list shows it."},
		},
		"required": []string{"action"},
	}}
}

func (t *BrowserTool) Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	out, _, err := t.ExecuteWithImages(ctx, execCtx, argsJSON)
	return out, err
}

// ExecuteWithImages runs one action and returns a screenshot's capture
// alongside the text, so the agent can send it to the model with the result
// (see ImageTool). Callers with no way to carry a picture — the tests, the
// paths that only want a tool's text — use Execute and lose the image.
func (t *BrowserTool) ExecuteWithImages(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, []protocol.ImageData, error) {
	var a browserArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "", nil, fmt.Errorf("invalid arguments: %w", err)
	}
	known := false
	for _, v := range browserActions {
		known = known || v == a.Action
	}
	switch {
	case !known:
		return "", nil, fmt.Errorf("unknown action %q", a.Action)
	case a.Action == "navigate" && a.URL == "":
		return "", nil, fmt.Errorf("navigate needs a url")
	case a.Action == "navigate" && !isHTTPURL(a.URL):
		return "", nil, fmt.Errorf("navigate only opens http(s) URLs")
	case (a.Action == "click" || a.Action == "type" || a.Action == "hover" || a.Action == "select") && a.Ref == "":
		return "", nil, fmt.Errorf("%s needs a ref from the latest snapshot", a.Action)
	case a.Action == "select" && a.Text == "":
		return "", nil, fmt.Errorf("select needs the option's label or value as text")
	case a.Action == "wait" && a.Ms <= 0 && a.Text == "":
		return "", nil, fmt.Errorf("wait needs ms, text, or both")
	case a.Action == "wait" && a.Ms > maxBrowserWaitMs:
		return "", nil, fmt.Errorf("wait takes at most %d ms", maxBrowserWaitMs)
	case a.Action == "tabs" && a.Op != "list" && a.Op != "new" && a.Op != "select" && a.Op != "close":
		return "", nil, fmt.Errorf("tabs needs op list, new, select or close")
	case a.Action == "tabs" && (a.Op == "select" || a.Op == "close") && a.Tab < 1:
		return "", nil, fmt.Errorf("tabs %s needs the tab's number, from 1", a.Op)
	case a.Action == "press" && a.Key == "":
		return "", nil, fmt.Errorf("press needs a key")
	case a.Action == "evaluate" && a.Expression == "":
		return "", nil, fmt.Errorf("evaluate needs an expression")
	}
	if t.relay == nil {
		return "", nil, fmt.Errorf("no browser is available")
	}
	forwarded, err := json.Marshal(a)
	if err != nil {
		return "", nil, err
	}
	res, err := t.relay.Browse(ctx, execCtx.SessionID, a.Action, forwarded)
	if err != nil {
		// The host's error can quote page-controlled text (a script exception, say).
		return "", nil, errors.New(WrapUntrusted("the browser page", err.Error()))
	}
	out := WrapUntrusted("the browser page", res.Output)
	if res.Image == nil {
		if a.Action == "screenshot" {
			return "", nil, fmt.Errorf("the browser took no screenshot — its client may not support the screenshot action")
		}
		return out, nil, nil
	}
	if err := validateImage(res.Image); err != nil {
		return "", nil, err
	}
	return out, []protocol.ImageData{*res.Image}, nil
}

// validateImage checks a capture before it reaches a provider request. The
// browser host is a client's code, so nothing here is trusted: a bad mime type
// or oversized payload would otherwise fail deep inside a provider call, or
// sit in every following request.
func validateImage(img *protocol.ImageData) error {
	switch img.MimeType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return fmt.Errorf("screenshot type %q isn't one providers accept (png, jpeg, webp, gif)", img.MimeType)
	}
	if _, err := base64.StdEncoding.DecodeString(img.Data); err != nil {
		return fmt.Errorf("screenshot data isn't valid base64: %w", err)
	}
	if base64.StdEncoding.DecodedLen(len(img.Data)) > maxBrowserImageBytes {
		return fmt.Errorf("screenshot is larger than %d MB — downscale it before sending (SPEC.md §4e)", maxBrowserImageBytes>>20)
	}
	return nil
}
