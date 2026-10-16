package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is what yoakeland tells servers it is.
var Version = "dev"

// Protocol revisions this client speaks.
const (
	protoModern = "2026-07-28"
	protoLegacy = "2025-11-25"
)

var legacyVersions = map[string]bool{"2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaInfo    = "io.modelcontextprotocol/clientInfo"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
)

// ServerInfo is what a server said about itself.
type ServerInfo struct {
	Name         string `json:"name,omitempty"`
	Title        string `json:"title,omitempty"`
	Version      string `json:"version,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// Protocol is the revision in use.
	Protocol string `json:"protocol,omitempty"`
	// Tools, Resources and Prompts say what the server offers.
	Tools     bool `json:"tools,omitempty"`
	Resources bool `json:"resources,omitempty"`
	Prompts   bool `json:"prompts,omitempty"`
}

// limitInfo caps what a server says about itself. It goes to every peer on each
// status push, so a server can't make that push arbitrarily large.
func limitInfo(info ServerInfo) ServerInfo {
	info.Name, info.Title, info.Version = shorten(info.Name, 200), shorten(info.Title, 200), shorten(info.Version, 200)
	if len(info.Instructions) > 2048 {
		info.Instructions = strings.ToValidUTF8(info.Instructions[:2048], "") + "…"
	}
	return info
}

type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
}

// ToolInfo is one tool as a server lists it. Everything in it came from the
// server, so none of it is trusted.
type ToolInfo struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ContentBlock is one piece of a tool result.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64 (image, audio)
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"`
	} `json:"resource,omitempty"`
}

// CallResult is what tools/call returned.
type CallResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type ResourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Options is how to reach one server.
type Options struct {
	Config ServerConfig // variables already expanded
	Dir    string       // working directory of a stdio server
	// Bearer supplies an OAuth access token for a remote server.
	Bearer bearerFunc
	// OnToolsChanged is told the server's tool list changed.
	OnToolsChanged func()
	// Getenv reads the daemon's environment (os.Getenv; tests replace it).
	Getenv func(string) string
}

// Client is a connection to one server.
type Client struct {
	opt  Options
	t    transport
	era  string // "legacy" or "modern"
	next atomic.Int64

	mu       sync.Mutex
	info     ServerInfo
	version  string
	reinit   sync.Mutex // one session restart at a time
	listenCt context.CancelFunc
}

var errInputRequired = errors.New("the server asked for input, which yoakeland doesn't support yet")

// Dial connects and negotiates a protocol revision. Cancelling ctx abandons
// the connect; the connected client does not depend on it.
func Dial(ctx context.Context, opt Options) (*Client, error) {
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	c := &Client{opt: opt}
	if err := c.connect(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) openTransport(ctx context.Context, forceSSE bool) (transport, error) {
	cfg := c.opt.Config
	switch typ := cfg.EffectiveType(); {
	case typ == TypeStdio:
		dir := c.opt.Dir
		if cfg.Cwd != "" {
			dir = cfg.Cwd
		}
		env := baseEnv(c.opt.Getenv)
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		return startStdio(cfg.Command, cfg.Args, dir, env, c.handle)
	case typ == TypeSSE || forceSSE:
		return connectSSE(ctx, cfg.URL, cfg.Headers, c.opt.Bearer, c.handle)
	default:
		return newHTTP(cfg.URL, cfg.Headers, c.opt.Bearer, c.handle), nil
	}
}

func (c *Client) connect(ctx context.Context) error {
	t, err := c.openTransport(ctx, false)
	if err != nil {
		return err
	}
	c.t = t
	err = c.initialize(ctx)
	if err == nil {
		return nil
	}
	var auth *errNeedsAuth
	if errors.As(err, &auth) {
		return err
	}
	// Refused rather than unreachable: the server may speak only the modern revision.
	var rerr *rpcError
	var herr *httpError
	if errors.As(err, &rerr) || errors.As(err, &herr) {
		if derr := c.discover(ctx); derr == nil {
			return nil
		} else if errors.As(derr, &auth) {
			return derr
		}
		// A streamable-HTTP server that answers neither is perhaps an older SSE one.
		if herr != nil && c.opt.Config.EffectiveType() == TypeHTTP && (herr.Status == 400 || herr.Status == 404 || herr.Status == 405) {
			c.t.close()
			if st, serr := c.openTransport(ctx, true); serr == nil {
				c.t = st
				if ierr := c.initialize(ctx); ierr == nil {
					return nil
				}
				st.close()
			}
		}
	}
	if d := c.t.diag(); d != "" && !strings.Contains(err.Error(), d) {
		err = fmt.Errorf("%w\n%s", err, d)
	}
	return err
}

func (c *Client) clientInfo() map[string]any {
	return map[string]any{"name": "yoakeland", "title": "Yoakeland", "version": Version}
}

func (c *Client) initialize(ctx context.Context) error {
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := newRequest(c.next.Add(1), "initialize", map[string]any{
		"protocolVersion": protoLegacy,
		"capabilities":    map[string]any{},
		"clientInfo":      c.clientInfo(),
	})
	resp, err := c.t.call(ictx, req)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools     json.RawMessage `json:"tools"`
			Resources json.RawMessage `json:"resources"`
			Prompts   json.RawMessage `json:"prompts"`
		} `json:"capabilities"`
		ServerInfo   ServerInfo `json:"serverInfo"`
		Instructions string     `json:"instructions"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return fmt.Errorf("the server's initialize answer is malformed: %w", err)
	}
	if !legacyVersions[res.ProtocolVersion] {
		return fmt.Errorf("the server uses protocol revision %q, which yoakeland doesn't support", res.ProtocolVersion)
	}
	if err := c.t.notify(ictx, newNotification("notifications/initialized", nil)); err != nil {
		return err
	}
	info := res.ServerInfo
	info.Instructions = res.Instructions
	info.Protocol = res.ProtocolVersion
	info.Tools = len(res.Capabilities.Tools) > 0
	info.Resources = len(res.Capabilities.Resources) > 0
	info.Prompts = len(res.Capabilities.Prompts) > 0
	c.mu.Lock()
	c.era, c.version, c.info = "legacy", res.ProtocolVersion, limitInfo(info)
	c.mu.Unlock()
	if ht, ok := c.t.(*httpTransport); ok {
		ht.setVersion(res.ProtocolVersion, false)
		lctx, lcancel := context.WithCancel(context.Background())
		c.mu.Lock()
		if c.listenCt != nil { // a new session replaces the listener of the old one
			c.listenCt()
		}
		c.listenCt = lcancel
		c.mu.Unlock()
		go ht.listen(lctx)
	}
	return nil
}

// discover is the modern revision's opening, and the probe that tells a modern
// server from a legacy one that refused initialize.
func (c *Client) discover(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.mu.Lock()
	c.era, c.version = "modern", protoModern
	c.mu.Unlock()
	if ht, ok := c.t.(*httpTransport); ok {
		ht.setVersion(protoModern, true)
	}
	var res struct {
		SupportedVersions []string        `json:"supportedVersions"`
		Capabilities      map[string]any  `json:"capabilities"`
		Instructions      string          `json:"instructions"`
		Meta              json.RawMessage `json:"_meta"`
	}
	if err := c.rpc(dctx, "server/discover", map[string]any{}, &res); err != nil {
		c.mu.Lock()
		c.era, c.version = "", ""
		c.mu.Unlock()
		if ht, ok := c.t.(*httpTransport); ok {
			ht.setVersion("", false)
		}
		return err
	}
	ok := false
	for _, v := range res.SupportedVersions {
		ok = ok || v == protoModern
	}
	if !ok {
		return fmt.Errorf("the server supports protocol revisions %v, not %s", res.SupportedVersions, protoModern)
	}
	info := ServerInfo{Instructions: res.Instructions, Protocol: protoModern}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(res.Meta, &meta)
	_ = json.Unmarshal(meta["io.modelcontextprotocol/serverInfo"], &info)
	info.Instructions, info.Protocol = res.Instructions, protoModern
	_, info.Tools = res.Capabilities["tools"]
	_, info.Resources = res.Capabilities["resources"]
	_, info.Prompts = res.Capabilities["prompts"]
	c.mu.Lock()
	c.info = limitInfo(info)
	c.mu.Unlock()
	return nil
}

// Info is what the server said about itself, and the protocol in use.
func (c *Client) Info() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Done is closed when the connection is gone.
func (c *Client) Done() <-chan struct{} { return c.t.done() }

// Diag is the server's own account of why it failed, if it gave one.
func (c *Client) Diag() string { return c.t.diag() }

// Close ends the connection and, for a local server, its process.
func (c *Client) Close() {
	c.mu.Lock()
	if c.listenCt != nil {
		c.listenCt()
	}
	c.mu.Unlock()
	if c.t != nil {
		c.t.close()
	}
}

// rpc makes one request. A modern request carries its version and identity in
// _meta; a legacy HTTP request whose session expired is retried once on a new one.
func (c *Client) rpc(ctx context.Context, method string, params map[string]any, out any) error {
	for attempt := 0; ; attempt++ {
		used := ""
		if ht, ok := c.t.(*httpTransport); ok {
			used = ht.sessionID()
		}
		err := c.rpcOnce(ctx, method, params, out)
		// A call that was sent while another call was renewing the session goes
		// out without one, and the server answers that 404 too.
		var herr *httpError
		gone := errors.Is(err, errSessionExpired) || (used != "" && errors.As(err, &herr) && herr.Status == http.StatusNotFound)
		if gone && attempt == 0 && method != "initialize" {
			if rerr := c.restartSession(ctx, used); rerr != nil {
				return rerr
			}
			continue
		}
		return err
	}
}

// restartSession starts a new session, unless another call already replaced the
// one that failed (failed) while this one waited for the lock.
func (c *Client) restartSession(ctx context.Context, failed string) error {
	c.reinit.Lock()
	defer c.reinit.Unlock()
	ht, ok := c.t.(*httpTransport)
	if !ok {
		return errSessionExpired
	}
	if ht.sessionID() != failed {
		return nil
	}
	// The old session stays until the new one is known, so a call sent meanwhile
	// fails the same way and waits here for this restart rather than seeing none.
	err := c.initialize(ctx)
	if err == nil && ht.sessionID() == failed {
		ht.setSession("") // the server gave no new session
	}
	return err
}

func (c *Client) rpcOnce(ctx context.Context, method string, params map[string]any, out any) error {
	c.mu.Lock()
	modern := c.era == "modern"
	c.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	if modern {
		meta, _ := params["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[metaVersion], meta[metaInfo], meta[metaCaps] = protoModern, c.clientInfo(), map[string]any{}
		params["_meta"] = meta
	}
	req, err := newRequest(c.next.Add(1), method, params)
	if err != nil {
		return err
	}
	resp, err := c.t.call(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			c.cancelled(req)
		}
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	var kind struct {
		ResultType string `json:"resultType"`
	}
	_ = json.Unmarshal(resp.Result, &kind)
	if kind.ResultType == "input_required" {
		return errInputRequired
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("%s: the server's answer is malformed: %w", method, err)
	}
	return nil
}

// cancelled tells the server a request is no longer wanted. Over HTTP in the
// modern revision, dropping the response stream already says so.
func (c *Client) cancelled(req *message) {
	c.mu.Lock()
	modernHTTP := c.era == "modern"
	c.mu.Unlock()
	if _, isHTTP := c.t.(*httpTransport); isHTTP && modernHTTP {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var id any
	_ = json.Unmarshal(req.ID, &id)
	_ = c.t.notify(ctx, newNotification("notifications/cancelled", map[string]any{"requestId": id, "reason": "the turn was stopped"}))
}

// handle answers what the server sends on its own initiative.
func (c *Client) handle(m *message) *message {
	if m.isRequest() {
		switch m.Method {
		case "ping":
			return replyResult(m, map[string]any{})
		case "roots/list":
			return replyResult(m, map[string]any{"roots": []any{}})
		}
		// Sampling and elicitation would have a model or a person answer for the
		// server; yoakeland offers neither, and says so.
		return replyError(m, errMethodNotFound, "yoakeland doesn't support "+m.Method)
	}
	if m.Method == "notifications/tools/list_changed" && c.opt.OnToolsChanged != nil {
		c.opt.OnToolsChanged()
	}
	return nil
}

const maxTools = 1000

// ListTools returns every tool the server offers.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.rpc(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || len(all) >= maxTools {
			break
		}
		cursor = res.NextCursor
	}
	if len(all) > maxTools {
		all = all[:maxTools]
	}
	return all, nil
}

// CallTool runs a tool. args is the JSON object the model wrote; headers from
// tool parameters (the modern HTTP transport's x-mcp-header) are taken from tool.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	var a any = map[string]any{}
	if len(trimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("the arguments aren't valid JSON: %w", err)
		}
	}
	if a == nil {
		a = map[string]any{}
	}
	if _, ok := a.(map[string]any); !ok {
		return nil, errors.New("the arguments must be a JSON object")
	}
	var res CallResult
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": a}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

const maxResources = 500

// ListResources returns the resources the server offers (not its templates).
func (c *Client) ListResources(ctx context.Context) ([]ResourceInfo, error) {
	var all []ResourceInfo
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Resources  []ResourceInfo `json:"resources"`
			NextCursor string         `json:"nextCursor"`
		}
		if err := c.rpc(ctx, "resources/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Resources...)
		if res.NextCursor == "" || len(all) >= maxResources {
			break
		}
		cursor = res.NextCursor
	}
	if len(all) > maxResources {
		all = all[:maxResources]
	}
	return all, nil
}

// ReadResource reads one resource.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ContentBlock, error) {
	var res struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := c.rpc(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return nil, err
	}
	out := make([]ContentBlock, 0, len(res.Contents))
	for _, r := range res.Contents {
		b := ContentBlock{Type: "text", Text: r.Text, URI: r.URI, MimeType: r.MimeType}
		if r.Blob != "" {
			b = ContentBlock{Type: "blob", Data: r.Blob, URI: r.URI, MimeType: r.MimeType}
		}
		out = append(out, b)
	}
	return out, nil
}
