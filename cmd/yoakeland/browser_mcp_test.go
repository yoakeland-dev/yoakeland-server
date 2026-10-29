package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type mcpRelay struct{ session, action string }

func (r *mcpRelay) Browse(_ context.Context, sessionID, action string, _ json.RawMessage) (tool.BrowserResult, error) {
	r.session, r.action = sessionID, action
	res := tool.BrowserResult{Output: "page"}
	if action == "screenshot" {
		res.Image = &protocol.ImageData{MimeType: "image/png", Data: "AAAA"}
	}
	return res, nil
}

// A CLI agent reaches the browser through the MCP bridge: only with its chat's token, as that chat,
// and a screenshot comes back as an image part.
func TestBrowserMCP_ServesTheToolToItsChat(t *testing.T) {
	relay := &mcpRelay{}
	m := newBrowserMCP(relay)
	srv := m.serversFor("chat-1")
	if len(srv) != 1 || srv[0].Type != "http" {
		t.Fatalf("serversFor = %+v", srv)
	}
	if again := m.serversFor("chat-1"); again[0].Headers["Authorization"] != srv[0].Headers["Authorization"] {
		t.Fatal("a chat's token changed between turns")
	}
	call := func(auth, body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv[0].URL, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := call("", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	auth := srv[0].Headers["Authorization"]
	if code, _ := call(auth, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	_, list := call(auth, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(toJSON(list), `"name":"browser"`) {
		t.Fatalf("tools/list = %v", list)
	}
	_, shot := call(auth, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"browser","arguments":{"action":"screenshot"}}}`)
	if relay.session != "chat-1" || relay.action != "screenshot" || !strings.Contains(toJSON(shot), `"type":"image"`) {
		t.Fatalf("session=%q action=%q result=%v", relay.session, relay.action, shot)
	}
	_, bad := call(auth, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"browser","arguments":{"action":"select"}}}`)
	if !strings.Contains(toJSON(bad), `"isError":true`) {
		t.Fatalf("invalid args should be a tool error: %v", bad)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
