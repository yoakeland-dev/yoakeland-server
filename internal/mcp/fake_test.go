package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an MCP server in either protocol era. handle maps one request
// to its response (nil for a notification), so the same server runs over
// stdio (TestMain) and HTTP (newHTTPFake).
type fakeServer struct {
	era string // "legacy" or "modern"

	mu        sync.Mutex
	cancelled []string // ids it was told to stop
	calls     []string
	listCalls int
}

func (f *fakeServer) tools() []ToolInfo {
	ro := true
	return []ToolInfo{
		{Name: "echo", Description: "Echo the text back.", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)},
		{Name: "lookup", Description: "Read-only lookup.", Annotations: &ToolAnnotations{ReadOnlyHint: &ro}, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "fail", Description: "Always fails.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "structured", Description: "Structured only.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "image", Description: "An image.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "slow", Description: "Takes forever.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "env", Description: "Prints the environment.", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}

func (f *fakeServer) handle(m *message) *message {
	if m.Method == "notifications/cancelled" {
		var p struct {
			RequestID any `json:"requestId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.mu.Lock()
		f.cancelled = append(f.cancelled, fmt.Sprint(p.RequestID))
		f.mu.Unlock()
		return nil
	}
	if !m.isRequest() {
		return nil
	}
	var params map[string]any
	_ = json.Unmarshal(m.Params, &params)
	if f.era == "modern" {
		if m.Method == "initialize" {
			return replyError(m, errMethodNotFound, "initialize isn't a method; this server speaks 2026-07-28")
		}
		meta, _ := params["_meta"].(map[string]any)
		if meta[metaVersion] != protoModern {
			return &message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32022, Message: "Unsupported protocol version", Data: json.RawMessage(`{"supported":["2026-07-28"]}`)}}
		}
	}
	switch m.Method {
	case "initialize":
		return replyResult(m, map[string]any{
			"protocolVersion": params["protocolVersion"],
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1.2.3"},
			"instructions":    "Be nice.",
		})
	case "server/discover":
		return replyResult(m, map[string]any{
			"resultType": "complete", "supportedVersions": []string{protoModern},
			"capabilities": map[string]any{"tools": map[string]any{}},
			"_meta":        map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "fake-modern", "version": "2.0"}},
			"instructions": "Modern.",
		})
	case "tools/list":
		f.mu.Lock()
		f.listCalls++
		f.mu.Unlock()
		all := f.tools()
		if params["cursor"] == nil {
			return replyResult(m, map[string]any{"resultType": "complete", "tools": all[:3], "nextCursor": "p2"})
		}
		return replyResult(m, map[string]any{"resultType": "complete", "tools": all[3:]})
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		f.mu.Lock()
		f.calls = append(f.calls, name)
		f.mu.Unlock()
		switch name {
		case "echo":
			return replyResult(m, map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(args["text"])}}})
		case "fail":
			return replyResult(m, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "it broke"}}})
		case "structured":
			return replyResult(m, map[string]any{"content": []any{}, "structuredContent": map[string]any{"n": 3}})
		case "image":
			return replyResult(m, map[string]any{"content": []any{map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"}}})
		case "env":
			return replyResult(m, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Join(os.Environ(), "\n")}}})
		case "slow":
			time.Sleep(10 * time.Second)
			return replyResult(m, map[string]any{"content": []any{}})
		}
		return replyError(m, errInvalidParams, "unknown tool: "+name)
	case "resources/list":
		return replyResult(m, map[string]any{"resources": []any{map[string]any{"uri": "mem://a", "name": "A"}}})
	case "resources/read":
		return replyResult(m, map[string]any{"contents": []any{map[string]any{"uri": params["uri"], "mimeType": "text/plain", "text": "alpha"}}})
	}
	return replyError(m, errMethodNotFound, "no method "+m.Method)
}

// TestMain turns the test binary into a stdio fake server when asked to.
func TestMain(m *testing.M) {
	if era := os.Getenv("MCP_FAKE_STDIO"); era != "" {
		f := &fakeServer{era: era}
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 1<<20), maxFrame)
		var wmu sync.Mutex
		for sc.Scan() {
			msgs, err := parseMessages(sc.Bytes())
			if err != nil {
				continue
			}
			for _, msg := range msgs {
				go func(msg *message) {
					if resp := f.handle(msg); resp != nil {
						b, _ := json.Marshal(resp)
						wmu.Lock()
						fmt.Fprintf(os.Stdout, "%s\n", b)
						wmu.Unlock()
					}
				}(msg)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func stdioConfig(era string, env map[string]string) ServerConfig {
	e := map[string]string{"MCP_FAKE_STDIO": era}
	for k, v := range env {
		e[k] = v
	}
	return ServerConfig{Command: os.Args[0], Env: e}
}

// httpFake serves fakeServer over Streamable HTTP, optionally with sessions,
// SSE-framed responses, and a bearer-token requirement.
type httpFake struct {
	*fakeServer
	srv      *httptest.Server
	sessions bool
	sse      bool
	token    string

	mu      sync.Mutex
	headers []http.Header
	inits   int
}

func newHTTPFake(t *testing.T, era string, sessions, sse bool, token string) *httpFake {
	t.Helper()
	h := &httpFake{fakeServer: &fakeServer{era: era}, sessions: sessions, sse: sse, token: token}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

// initializes is how many times a client has initialized a session.
func (h *httpFake) initializes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inits
}

func (h *httpFake) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.headers = append(h.headers, r.Header.Clone())
	h.mu.Unlock()
	if h.token != "" && r.Header.Get("Authorization") != "Bearer "+h.token {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+h.srv.URL+`/.well-known/oauth-protected-resource", scope="read"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var m message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if h.era == "modern" && r.Header.Get("MCP-Protocol-Version") != protoModern {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32020,"message":"header mismatch"}}`))
		return
	}
	if h.sessions && m.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if m.Method == "initialize" {
		h.mu.Lock()
		h.inits++
		h.mu.Unlock()
	}
	resp := h.handle(&m)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if h.sessions && m.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", "sess-1")
	}
	b, _ := json.Marshal(resp)
	if h.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// A progress notification first, as a streaming server would send.
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
