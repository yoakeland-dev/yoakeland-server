package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"yoakeland/internal/artifact"
	"yoakeland/internal/protocol"

	"yoakeland/internal/tool"
)

// browserMCP serves the browser tool to CLI agents (Claude Code and friends), which run their own
// tools and are never handed yoakeland's: it is an MCP server on a loopback port, and each chat's
// agent gets it in its --mcp-config with a token that names the chat. A call goes through the same
// BrowserTool and browserBroker the API models use, so it drives the panel the user is watching.
// The agent's own permission prompt stands in for the tool's (navigate off-site, evaluate).
// ponytail: plain JSON responses over POST, no SSE stream; add one if a client insists on it.
type browserMCP struct {
	tool *tool.BrowserTool

	mu        sync.Mutex
	ln        net.Listener
	token     map[string]string // chat -> token
	bySession map[string]string // token -> chat
}

func newBrowserMCP(relay tool.BrowserRelay) *browserMCP {
	return &browserMCP{tool: tool.NewBrowserTool(relay), token: map[string]string{}, bySession: map[string]string{}}
}

// serversFor is the MCP entry that gives sessionID's agent the browser, starting the listener on first use.
func (m *browserMCP) serversFor(sessionID string) []tool.AgentServer {
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.listen() {
		return nil
	}
	tok := m.token[sessionID]
	if tok == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return nil
		}
		tok = hex.EncodeToString(b)
		m.token[sessionID], m.bySession[tok] = tok, sessionID
	}
	return []tool.AgentServer{{
		Name: "yoake-browser", Type: "http", URL: "http://" + m.ln.Addr().String() + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + tok},
	}}
}

// listen starts the loopback listener if it isn't up; m.mu is held.
func (m *browserMCP) listen() bool {
	if m.ln == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Printf("browser MCP: %v", err)
			return false
		}
		m.ln = ln
		go func() { _ = http.Serve(ln, m) }()
	}
	return true
}

// port is the listener's port, starting it if need be, so the server browser can be kept away from it.
func (m *browserMCP) port() uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.listen() {
		return 0
	}
	return uint16(m.ln.Addr().(*net.TCPAddr).Port)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (m *browserMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	sessionID := m.bySession[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	m.mu.Unlock()
	if sessionID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req rpcRequest
	if err != nil || json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any, rpcErr map[string]any) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			out["error"] = rpcErr
		} else {
			out["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-03-26"
		}
		reply(map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "yoake-browser", "version": "1"},
		}, nil)
	case "ping":
		reply(map[string]any{}, nil)
	case "tools/list":
		tools := []any{}
		for _, d := range m.definitions() {
			tools = append(tools, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters})
		}
		reply(map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			reply(nil, map[string]any{"code": -32602, "message": "bad params"})
			return
		}
		if p.Name == tool.TodoToolName || p.Name == tool.ArtifactLogsToolName {
			out, err := m.runLight(p.Name, sessionID, p.Arguments)
			if err != nil {
				reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}, nil)
				return
			}
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": out}}}, nil)
			return
		}
		if p.Name != "browser" {
			reply(nil, map[string]any{"code": -32602, "message": "unknown tool"})
			return
		}
		out, images, err := m.tool.ExecuteWithImages(r.Context(), tool.ExecutionContext{SessionID: sessionID}, p.Arguments)
		if err != nil {
			reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}, nil)
			return
		}
		content := []any{map[string]any{"type": "text", "text": out}}
		for _, img := range images {
			content = append(content, map[string]any{"type": "image", "data": img.Data, "mimeType": img.MimeType})
		}
		reply(map[string]any{"content": content}, nil)
	default:
		reply(nil, map[string]any{"code": -32601, "message": "method not found"})
	}
}

// MCPToolPrefix is how a CLI agent names this server's tools: mcp__<server>__<tool>.
const mcpToolPrefix = "mcp__yoake-browser__"

// definitions are the tools this server offers: the browser, and two that only exist for CLI agents
// (an API model has them as its own tools): the todo list, and an artifact page's log. The agent
// itself records a todo_write call it makes (agent.recordTodos); here it only has to succeed.
func (m *browserMCP) definitions() []tool.ToolDefinition {
	return []tool.ToolDefinition{
		m.tool.Definition(),
		tool.NewTodoWriteTool(nil).Definition(),
		tool.NewArtifactLogsTool(artifact.Default).Definition(),
	}
}

// runLight runs the two tools that need nothing from the browser.
func (m *browserMCP) runLight(name, sessionID string, args json.RawMessage) (string, error) {
	if name == tool.TodoToolName {
		items, err := protocol.ParseTodoWrite(args)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("todo list updated (%d items)", len(items)), nil
	}
	return tool.NewArtifactLogsTool(artifact.Default).Execute(context.Background(), tool.ExecutionContext{SessionID: sessionID}, args)
}
