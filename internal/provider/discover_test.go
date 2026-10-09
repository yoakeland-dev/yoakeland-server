package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func useDiscovery(t *testing.T, f *liveFake, script func(int, map[string]any, io.Writer)) {
	t.Helper()
	old := startDiscovery
	startDiscovery = f.starter(script)
	t.Cleanup(func() { startDiscovery = old })
}

// Shaped like what `claude` answered an initialize request with.
var claudeInit = []any{
	map[string]any{"value": "default", "resolvedModel": "claude-sonnet-5-5", "displayName": "Default (recommended)", "description": "Sonnet 5.5", "supportsEffort": true,
		"supportedEffortLevels": []any{"low", "medium", "high", "xhigh", "max"}, "supportsAutoMode": true},
	map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus 5.5", "supportsEffort": true,
		"supportedEffortLevels": []any{"low", "medium", "high", "xhigh", "max"}, "supportsFastMode": true, "supportsAutoMode": true},
	map[string]any{"value": "haiku", "resolvedModel": "claude-haiku-4-5-20251001", "displayName": "Haiku 4.5"},
}

func TestDiscoverClaudeModels(t *testing.T) {
	f := &liveFake{}
	useDiscovery(t, f, func(_ int, m map[string]any, out io.Writer) {
		if m["type"] == "control_request" && controlSubtype(m) == "initialize" {
			line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m["request_id"], "response": map[string]any{"models": claudeInit}}})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := discoverClaudeModels(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].ID != "opus" || got[1].DisplayName != "Claude Opus 5.5" {
		t.Fatalf("models = %+v", got)
	}
	if !reflect.DeepEqual(got[1].Capabilities["reasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"}) || got[1].Capabilities["fastMode"] != true || got[1].Capabilities["autoMode"] != true {
		t.Fatalf("opus capabilities = %v", got[1].Capabilities)
	}
	if got[2].Capabilities["reasoningEfforts"] != nil || got[2].Capabilities["fastMode"] != nil {
		t.Fatalf("haiku claims what it doesn't support: %v", got[2].Capabilities)
	}
	f.waitClosed(t, 0) // the throwaway process was let go
}

func TestDiscoverClaudeModelsLoggedOut(t *testing.T) {
	f := &liveFake{}
	useDiscovery(t, f, func(_ int, m map[string]any, out io.Writer) {
		line(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m["request_id"], "response": map[string]any{"models": claudeInit, "account": map[string]any{"tokenSource": "none"}}}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := discoverClaudeModels(ctx, t.TempDir()); !errors.Is(err, errClaudeLoggedOut) {
		t.Fatalf("err = %v, want errClaudeLoggedOut", err)
	}
}

func TestDiscoverClaudeModelsProcessGone(t *testing.T) {
	f := &liveFake{}
	useDiscovery(t, f, func(_ int, m map[string]any, out io.Writer) { _ = out.(*io.PipeWriter).Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := discoverClaudeModels(ctx, t.TempDir()); err == nil {
		t.Fatal("no error from a process that never answered")
	}
}

func TestManifestAddsWhatClaudeDoesNotReport(t *testing.T) {
	discoveredModels := claudeModelsFrom([]claudeModelInfo{
		{Value: "opus", ResolvedModel: "claude-opus-5-5", DisplayName: "Opus 5.5", SupportedEffortLevels: []string{"low", "xhigh", "max"}},
		{Value: "haiku", ResolvedModel: "claude-haiku-4-5-20251001", DisplayName: "Haiku"},
		{Value: "claude-opus-4-6", ResolvedModel: "claude-opus-4-6", SupportedEffortLevels: []string{"low", "high", "max"}},
	})
	manifest := []ModelInfo{
		{ID: "claude-opus-5", Capabilities: map[string]any{"reasoningEfforts": []string{"low", "xhigh", "max", "ultracode"}, "contextWindows": []string{"200k", "1m"}}},
		{ID: "claude-opus-4-6", Capabilities: map[string]any{"reasoningEfforts": []string{"low", "high", "max"}, "contextWindows": []string{"200k", "1m"}}},
		{ID: "claude-haiku-4-5", IsLegacy: true},
	}
	got := withManifest(discoveredModels, manifest)
	if got[0].DisplayName != "Claude Opus 5.5" || got[0].IsLegacy || !got[1].IsLegacy {
		t.Fatalf("names/legacy = %q %v %v", got[0].DisplayName, got[0].IsLegacy, got[1].IsLegacy)
	}
	if e := got[0].Capabilities["reasoningEfforts"].([]string); e[len(e)-1] != "ultracode" {
		t.Fatalf("opus efforts = %v, want ultracode added", e)
	}
	if got[0].Capabilities["contextWindows"] == nil {
		t.Fatal("context windows weren't carried over")
	}
	if e := got[2].Capabilities["reasoningEfforts"].([]string); contains(e, "ultracode") {
		t.Fatalf("opus 4.6 got ultracode: %v", e)
	}
	if got[1].Capabilities["reasoningEfforts"] != nil {
		t.Fatalf("haiku gained efforts: %v", got[1].Capabilities)
	}
	// What the agent reported is untouched.
	if e := discoveredModels[0].Capabilities["reasoningEfforts"].([]string); contains(e, "ultracode") {
		t.Fatal("the input was modified")
	}
}

func TestDiscoverCodexModels(t *testing.T) {
	f := &liveFake{}
	page := 0
	useDiscovery(t, f, func(_ int, m map[string]any, out io.Writer) {
		reply := func(result any) { line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result}) }
		switch m["method"] {
		case "initialize":
			reply(map[string]any{})
		case "model/list":
			page++
			if page == 1 {
				reply(map[string]any{"nextCursor": "p2", "data": []any{
					map[string]any{"id": "gpt-b", "model": "gpt-b", "displayName": "GPT B", "hidden": true},
					map[string]any{"id": "gpt-a", "model": "gpt-a", "displayName": "GPT A", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "high"}},
						"serviceTiers": []any{map[string]any{"id": "fast"}}},
				}})
				return
			}
			if m["params"].(map[string]any)["cursor"] != "p2" {
				t.Errorf("second page asked for cursor %v", m["params"])
			}
			reply(map[string]any{"nextCursor": nil, "data": []any{
				map[string]any{"id": "gpt-c", "model": "gpt-c", "displayName": "GPT C", "isDefault": true},
			}})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := discoverCodexModels(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gpt-c" || got[1].ID != "gpt-a" {
		t.Fatalf("models = %+v (default first, hidden left out, both pages read)", got)
	}
	if !reflect.DeepEqual(got[1].Capabilities["reasoningEfforts"], []string{"low", "high"}) || got[1].Capabilities["fastMode"] != true {
		t.Fatalf("gpt-a capabilities = %v", got[1].Capabilities)
	}
	if got[0].Capabilities["fastMode"] != nil {
		t.Fatalf("gpt-c has no fast tier: %v", got[0].Capabilities)
	}
}

func TestDiscoveryCache(t *testing.T) {
	c := &discoveryCache{entries: map[CLIKind]*discoveryEntry{}}
	fallback := func() []ModelInfo { return []ModelInfo{{ID: "fallback"}} }
	calls := 0
	ok := func(context.Context) ([]ModelInfo, error) { calls++; return []ModelInfo{{ID: "live"}}, nil }

	if got := c.get(CLIKindClaude, ok, fallback); got[0].ID != "live" || calls != 1 {
		t.Fatalf("first call = %v, %d fetches", got, calls)
	}
	if got := c.get(CLIKindClaude, ok, fallback); got[0].ID != "live" || calls != 1 {
		t.Fatalf("a fresh entry was fetched again: %d", calls)
	}

	// Stale: the old answer comes back at once and a refresh runs behind it.
	c.entries[CLIKindClaude].at = time.Now().Add(-2 * discoveryTTL)
	refreshed := make(chan struct{})
	slow := func(context.Context) ([]ModelInfo, error) {
		defer close(refreshed)
		return []ModelInfo{{ID: "newer"}}, nil
	}
	if got := c.get(CLIKindClaude, slow, fallback); got[0].ID != "live" {
		t.Fatalf("stale call = %v", got)
	}
	<-refreshed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.get(CLIKindClaude, ok, fallback); got[0].ID == "newer" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.get(CLIKindClaude, ok, fallback); got[0].ID != "newer" {
		t.Fatalf("the refresh never landed: %v", got)
	}

	// A failure falls back, and isn't retried straight away.
	fails := 0
	bad := func(context.Context) ([]ModelInfo, error) { fails++; return nil, errors.New("not installed") }
	if got := c.get(CLIKindCodex, bad, fallback); got[0].ID != "fallback" || fails != 1 {
		t.Fatalf("failing call = %v, %d", got, fails)
	}
	if got := c.get(CLIKindCodex, bad, fallback); got[0].ID != "fallback" || fails != 1 {
		t.Fatalf("a failure was retried at once: %d", fails)
	}
}

func TestCodexDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"GPT-6-Luna":        "GPT-6 Luna",
		"GPT-5.6-Terra":     "GPT-5.6 Terra",
		"GPT-5.5":           "GPT-5.5",
		"GPT-5.1-Codex-Max": "GPT-5.1 Codex Max",
		"o3":                "o3",
	} {
		if got := codexDisplayName(in); got != want {
			t.Errorf("codexDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
