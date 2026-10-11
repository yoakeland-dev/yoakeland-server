package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"yoakeland/internal/tool"
)

// ToolPrefix starts the name of every tool an MCP server provides:
// mcp__<server>__<tool>, the form Claude Code uses, so a permission rule written
// for one reads the same in the other.
const ToolPrefix = "mcp__"

const (
	maxWireName    = 64 // what the model APIs allow
	maxDescription = 1500
	maxSchemaBytes = 64 << 10
	maxResultBytes = 1 << 20
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// wireName is the name the model sees: unsafe characters become _, and a name
// that would be too long (or that another of the server's tools already took)
// gets a short hash of the original so it stays distinct and stable.
func wireName(server, raw string, taken map[string]bool) string {
	base := ToolPrefix + server + "__"
	clean := strings.Trim(unsafeName.ReplaceAllString(raw, "_"), "_")
	if clean == "" {
		clean = "tool"
	}
	name := base + clean
	if len(name) > maxWireName || taken[name] || strings.Contains(clean, "__") {
		sum := sha256.Sum256([]byte(raw))
		suffix := "_" + hex.EncodeToString(sum[:3])
		keep := maxWireName - len(base) - len(suffix)
		if keep < 1 {
			keep = 1
		}
		if len(clean) > keep {
			clean = clean[:keep]
		}
		name = base + clean + suffix
	}
	taken[name] = true
	return name
}

// ParseWireName splits mcp__<server>__<tool>.
func ParseWireName(name string) (server, rest string, ok bool) {
	if !strings.HasPrefix(name, ToolPrefix) {
		return "", "", false
	}
	server, rest, ok = strings.Cut(strings.TrimPrefix(name, ToolPrefix), "__")
	return server, rest, ok && server != "" && rest != ""
}

// normalizeSchema makes whatever a server called its input schema into an
// object schema every model API accepts, or reports that it can't.
func normalizeSchema(raw json.RawMessage) (map[string]any, error) {
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("its input schema is %d KB, over the %d KB limit", len(raw)>>10, maxSchemaBytes>>10)
	}
	var s map[string]any
	if len(trimSpace(raw)) > 0 && string(trimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("its input schema isn't a JSON object")
		}
	}
	if s == nil {
		s = map[string]any{}
	}
	delete(s, "$schema")
	if t, _ := s["type"].(string); t != "object" {
		if s["type"] != nil {
			return nil, fmt.Errorf("its input schema has type %v, not object", s["type"])
		}
		s["type"] = "object"
	}
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	return s, nil
}

// mcpTool is one server tool, offered to the model as an ordinary tool.
type mcpTool struct {
	srv  *entry
	wire string
	raw  string // the server's own name for it
	info ToolInfo
	def  tool.ToolDefinition
}

func (t *mcpTool) Name() string                    { return t.wire }
func (t *mcpTool) Description() string             { return t.def.Description }
func (t *mcpTool) Definition() tool.ToolDefinition { return t.def }

// RequiresPermission is always true: a server's own claim about what a tool
// does (its annotations) is not a reason to run it unasked. What lets it
// through is a permission rule the user wrote, or autonomous mode.
func (t *mcpTool) RequiresPermission([]byte) bool { return true }

// ReadOnly is the server's claim. It only decides whether the tool is offered
// in read-only and plan modes; it never lets anything run without asking.
func (t *mcpTool) ReadOnly() bool {
	a := t.info.Annotations
	return a != nil && a.ReadOnlyHint != nil && *a.ReadOnlyHint
}

func (t *mcpTool) Execute(ctx context.Context, _ tool.ExecutionContext, args []byte) (string, error) {
	c, err := t.srv.mgr.ready(ctx, t.srv)
	if err != nil {
		return "", err
	}
	timeout := t.srv.mgr.callTimeout(t.srv)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := c.CallTool(cctx, t.raw, args)
	if err != nil {
		return "", t.srv.mgr.callFailed(t.srv, c, err, ctx, cctx)
	}
	text := renderResult(res)
	if res.IsError {
		return "", errors.New(tool.WrapUntrusted("the MCP server "+t.srv.name, text))
	}
	return tool.WrapUntrusted("the MCP server "+t.srv.name, text), nil
}

// renderResult turns a tool result into the text a model reads. An MCP
// tool's pictures and audio have no home on a Tool.Execute result — a string,
// unlike the browser tool's own tool.ImageTool — so they are named here, not
// dropped silently. (An MCP server that wants its model to look at a picture
// is out of reach either way: the image is the *server's* result content, not
// a tool_result yoakeland built.)
func renderResult(res *CallResult) string {
	var parts []string
	for _, b := range res.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s content (%s, %s) not shown: yoakeland can't pass it to the model]", b.Type, orUnknown(b.MimeType), humanBytes(len(b.Data)*3/4)))
		case "resource_link":
			parts = append(parts, fmt.Sprintf("[resource link: %s %s]", b.Name, b.URI))
		case "resource":
			if b.Resource == nil {
				break
			}
			if b.Resource.Text != "" {
				parts = append(parts, fmt.Sprintf("[resource %s]\n%s", b.Resource.URI, b.Resource.Text))
			} else {
				parts = append(parts, fmt.Sprintf("[resource %s (%s, %s) is binary and not shown]", b.Resource.URI, orUnknown(b.Resource.MimeType), humanBytes(len(b.Resource.Blob)*3/4)))
			}
		default:
			parts = append(parts, fmt.Sprintf("[%s content not shown]", b.Type))
		}
	}
	out := strings.Join(parts, "\n")
	if strings.TrimSpace(out) == "" && len(res.StructuredContent) > 0 && string(res.StructuredContent) != "null" {
		var pretty strings.Builder
		var v any
		if json.Unmarshal(res.StructuredContent, &v) == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			pretty.Write(b)
			out = pretty.String()
		}
	}
	if out == "" {
		out = "(the tool returned nothing)"
	}
	if len(out) > maxResultBytes {
		out = strings.ToValidUTF8(out[:maxResultBytes], "") + "\n[... truncated]"
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown type"
	}
	return s
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// resourceTool reads a resource a server offers. One per server that has any.
type resourceTool struct {
	srv  *entry
	wire string
	def  tool.ToolDefinition
}

const resourceToolName = "read_resource"

func (t *resourceTool) Name() string                    { return t.wire }
func (t *resourceTool) Description() string             { return t.def.Description }
func (t *resourceTool) Definition() tool.ToolDefinition { return t.def }
func (t *resourceTool) RequiresPermission([]byte) bool  { return true }
func (t *resourceTool) ReadOnly() bool                  { return true }

func (t *resourceTool) Execute(ctx context.Context, _ tool.ExecutionContext, args []byte) (string, error) {
	var a struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.URI == "" {
		return "", errors.New("give the uri of the resource to read")
	}
	c, err := t.srv.mgr.ready(ctx, t.srv)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	blocks, err := c.ReadResource(cctx, a.URI)
	if err != nil {
		return "", t.srv.mgr.callFailed(t.srv, c, err, ctx, cctx)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "blob" {
			parts = append(parts, fmt.Sprintf("[%s (%s, %s) is binary and not shown]", b.URI, orUnknown(b.MimeType), humanBytes(len(b.Data)*3/4)))
		} else {
			parts = append(parts, b.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if len(out) > maxResultBytes {
		out = strings.ToValidUTF8(out[:maxResultBytes], "") + "\n[... truncated]"
	}
	return tool.WrapUntrusted("the MCP server "+t.srv.name, out), nil
}
