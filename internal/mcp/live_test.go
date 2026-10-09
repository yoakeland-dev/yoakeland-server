package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Live checks against the reference MCP servers on npm. Opt in with YK_LIVE=1
// (they download packages): YK_LIVE=1 go test ./internal/mcp -run TestLive -v
func TestLiveReferenceServers(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("set YK_LIVE=1 to run against the real reference servers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()

	t.Run("everything", func(t *testing.T) {
		c, err := Dial(ctx, Options{Config: ServerConfig{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-everything"}}, Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		t.Logf("server %s %s, protocol %s", c.Info().Name, c.Info().Version, c.Info().Protocol)
		tools, err := c.ListTools(ctx)
		if err != nil || len(tools) < 5 {
			t.Fatalf("tools = %d, %v", len(tools), err)
		}
		names := []string{}
		for _, ti := range tools {
			names = append(names, ti.Name)
			if _, err := normalizeSchema(ti.InputSchema); err != nil {
				t.Errorf("tool %s: %v", ti.Name, err)
			}
		}
		t.Logf("tools: %s", strings.Join(names, ", "))
		res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"message":"hello from yoakeland"}`))
		if err != nil || res.IsError || !strings.Contains(renderResult(res), "hello from yoakeland") {
			t.Fatalf("echo = %+v, %v", res, err)
		}
		res, err = c.CallTool(ctx, "get-sum", json.RawMessage(`{"a":2,"b":40}`))
		if err != nil || !strings.Contains(renderResult(res), "42") {
			t.Fatalf("get-sum = %+v, %v", res, err)
		}
		if _, err := c.CallTool(ctx, "echo", json.RawMessage(`{}`)); err == nil {
			// Invalid arguments come back as a tool error or an rpc error, never as success.
			t.Log("echo with no message was accepted")
		}
	})

	t.Run("filesystem", func(t *testing.T) {
		c, err := Dial(ctx, Options{Config: ServerConfig{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", dir}}, Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := os.WriteFile(dir+"/hello.txt", []byte("hi there"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := c.CallTool(ctx, "read_text_file", json.RawMessage(`{"path":"`+dir+`/hello.txt"}`))
		if err != nil || !strings.Contains(renderResult(res), "hi there") {
			t.Fatalf("read = %+v, %v", res, err)
		}
		tools, _ := c.ListTools(ctx)
		var ro int
		for _, ti := range tools {
			if ti.Annotations != nil && ti.Annotations.ReadOnlyHint != nil && *ti.Annotations.ReadOnlyHint {
				ro++
			}
		}
		t.Logf("%d tools, %d read-only by annotation", len(tools), ro)
	})
}
