package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

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
	if m.ln == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Printf("browser MCP: %v", err)
			return nil
		}
		m.ln = ln
		go func() { _ = http.Serve(ln, m) }()
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
		d := m.tool.Definition()
		reply(map[string]any{"tools": []any{map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters}}}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Name != "browser" {
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
