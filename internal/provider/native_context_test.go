package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"yoakeland/internal/agent"
)

// Every native agent's turn usage is a sum over its requests (spend); the
// context figure must be the last request's alone, with the agent's window.

func TestClaudeReportsLastRequestAsContext(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir(), Start: f.starter(func(_ int, m map[string]any, out io.Writer) {
		if m["type"] != "user" {
			return
		}
		line(out, map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": []any{},
			"usage": map[string]any{"input_tokens": 5, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 20, "output_tokens": 7}}})
		line(out, map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": []any{},
			"usage": map[string]any{"input_tokens": 6, "cache_read_input_tokens": 1500, "cache_creation_input_tokens": 0, "output_tokens": 9}}})
		line(out, map[string]any{"type": "result", "subtype": "success", "result": "ok",
			"usage":      map[string]any{"input_tokens": 11, "cache_read_input_tokens": 2500, "cache_creation_input_tokens": 20, "output_tokens": 16},
			"modelUsage": map[string]any{"haiku": map[string]any{"contextWindow": 200000}, "sub": map[string]any{"contextWindow": 100000}}})
	})}
	chunks, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "go"))
	if err != nil {
		t.Fatal(err)
	}
	u := chunks[len(chunks)-1].Usage
	if u.InputTokens != 2531 || u.ContextTokens != 6+1500+9 || u.WindowTokens != 200000 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestCodexReportsLastRequestAsContext(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindCodex, Model: "gpt-5.5", Dir: t.TempDir(), Start: f.starter(func(_ int, m map[string]any, out io.Writer) {
		reply := func(result any) { line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result}) }
		usage := func(in, o int) {
			line(out, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{
				"last": map[string]any{"inputTokens": in, "outputTokens": o}, "modelContextWindow": 258400}}})
		}
		switch m["method"] {
		case "initialize", "account/rateLimits/read":
			reply(map[string]any{})
		case "thread/start":
			reply(map[string]any{"thread": map[string]any{"id": "th1"}})
		case "turn/start":
			reply(map[string]any{"turn": map[string]any{"id": "t1"}})
			usage(100, 10)
			usage(150, 20)
			line(out, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "t1", "status": "completed"}}})
		}
	})}
	chunks, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "go"))
	if err != nil {
		t.Fatal(err)
	}
	u := chunks[len(chunks)-1].Usage
	if u.InputTokens != 250 || u.OutputTokens != 30 || u.ContextTokens != 170 || u.WindowTokens != 258400 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestACPReportsUsageUpdateAsContext(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	base := acpScript(new(bool))
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Model: "m1", Dir: t.TempDir(), Start: f.starter(func(n int, m map[string]any, out io.Writer) {
		if m["method"] == "session/prompt" {
			line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s",
				"update": map[string]any{"sessionUpdate": "usage_update", "used": 12345, "size": 200000}}})
		}
		base(n, m, out)
	})}
	chunks, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "go"))
	if err != nil {
		t.Fatal(err)
	}
	u := chunks[len(chunks)-1].Usage
	if u.ContextTokens != 12345 || u.WindowTokens != 200000 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestPricingCache_ContextWindowAndOverrides(t *testing.T) {
	dir := t.TempDir()
	overrides := `{"My-Local-Model": {"inputPerMTok": 2, "outputPerMTok": 4, "cacheReadPerMTok": 0.5, "contextWindow": 64000},
		"gpt-4o": {"inputPerMTok": 1, "outputPerMTok": 1}}`
	if err := os.WriteFile(filepath.Join(dir, "pricing-overrides.json"), []byte(overrides), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := NewPricingCache(dir)

	if got := pc.ContextWindow("gpt-4o-mini"); got != 128000 {
		t.Errorf("max_input_tokens from the embedded table: got %d, want 128000", got)
	}
	if got := pc.ContextWindow("no-such-model"); got != 0 {
		t.Errorf("unknown model: got %d", got)
	}
	if got := pc.ContextWindow("my-local-model"); got != 64000 {
		t.Errorf("override window: got %d", got)
	}
	// 1M in (0.5M cached) + 1M out: 0.5*2 + 0.5*0.5 + 4.
	if usd, ok := pc.CostUSD("my-local-model", 1_000_000, 500_000, 0, 1_000_000); !ok || usd < 5.249 || usd > 5.251 {
		t.Errorf("override cost: got %v, %v", usd, ok)
	}
	// An override wins over the table's own rate for the same model.
	if usd, ok := pc.CostUSD("gpt-4o", 1_000_000, 0, 0, 1_000_000); !ok || usd < 1.999 || usd > 2.001 {
		t.Errorf("override should beat the table: got %v, %v", usd, ok)
	}
	// A malformed file is ignored, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "pricing-overrides.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := NewPricingCache(dir).Lookup("my-local-model"); ok {
		t.Error("malformed overrides should leave the model unpriced")
	}
}

func TestOpenRouterCatalogRemembersContextLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"Vendor/Some-Model","name":"Some","context_length":131072},{"id":"vendor/other"}]}`))
	}))
	defer srv.Close()
	if _, err := fetchModels(srv.URL, func(*http.Request) {}, "name", "openrouter"); err != nil {
		t.Fatal(err)
	}
	if got := OpenRouterContextLength("vendor/some-model"); got != 131072 {
		t.Errorf("got %d, want 131072", got)
	}
	if got := OpenRouterContextLength("vendor/other"); got != 0 {
		t.Errorf("a model without context_length: got %d", got)
	}
}
