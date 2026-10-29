package provider

import (
	"encoding/json"
	"testing"

	"yoakeland/internal/tool"
)

func TestCodexMCPResultSplitsTextAndImages(t *testing.T) {
	text, imgs := codexMCPResult(json.RawMessage(`{"content":[{"type":"text","text":"Screenshot of x"},{"type":"image","data":"QUJD","mimeType":"image/jpeg"}]}`))
	if text != "Screenshot of x" || len(imgs) != 1 || imgs[0].MimeType != "image/jpeg" || imgs[0].Data != "QUJD" {
		t.Fatalf("got %q %v", text, imgs)
	}
	if text, _ := codexMCPResult(json.RawMessage(`null`)); text != "null" {
		t.Fatalf("unreadable result changed: %q", text)
	}
}

func TestCodexMCPConfig(t *testing.T) {
	if codexMCPConfig(nil) != nil {
		t.Fatal("no servers should add no config")
	}
	cfg := codexMCPConfig([]tool.AgentServer{{Name: "b", Type: "http", URL: "http://x/mcp", Headers: map[string]string{"Authorization": "Bearer t"}}})
	e, _ := cfg["b"].(map[string]any)
	if e["url"] != "http://x/mcp" || e["http_headers"] == nil {
		t.Fatalf("got %v", cfg)
	}
}
