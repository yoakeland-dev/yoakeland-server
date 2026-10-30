package provider_test

import (
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/provider"
)

func TestProviderStore_RegisterGetListRemove(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}

	cfg := provider.ProviderConfig{
		Name:         "my-anthropic",
		Kind:         provider.ProviderKindAPIKey,
		HTTPProvider: "anthropic",
		APIKey:       "sk-ant-secret",
		AuthMethod:   "manual",
	}
	if err := store.Register(cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := store.Get("my-anthropic")
	if !ok {
		t.Fatal("expected Get to find the registered provider")
	}
	if got.APIKey != "sk-ant-secret" {
		t.Fatalf("Get should return the real credential for internal use, got %q", got.APIKey)
	}

	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 listed provider, got %d", len(list))
	}
	if list[0].APIKey != "" {
		t.Fatalf("List must scrub credentials since it feeds a frontend picker, got APIKey %q", list[0].APIKey)
	}
	if list[0].Name != "my-anthropic" {
		t.Fatalf("unexpected listed provider: %+v", list[0])
	}

	if err := store.Remove("my-anthropic"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := store.Get("my-anthropic"); ok {
		t.Fatal("expected provider to be gone after Remove")
	}
}

// Each registration of a coding-agent CLI is its own account (see account.go): re-using
// a Name replaces that entry, a different Name adds one.
func TestProviderStore_CLIRegistrationsAreAccounts(t *testing.T) {
	store, err := provider.NewProviderStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	for _, n := range []string{"my-claude", "other-claude", "my-claude"} {
		if err := store.Register(provider.ProviderConfig{Name: n, Kind: provider.ProviderKindCLIClaude}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if got := len(store.List()); got != 2 {
		t.Errorf("expected 2 accounts (a re-used name replaces), got %d", got)
	}
}

func TestProviderStore_RegisterRequiresName(t *testing.T) {
	store, err := provider.NewProviderStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Kind: provider.ProviderKindAPIKey}); err == nil {
		t.Fatal("expected an error registering a provider with no name")
	}
}

func TestProviderStore_PersistsAtRestrictedPermissions(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, APIKey: "secret"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	info, err := os.Stat(filepath.Join(tempDir, "providers.json"))
	if err != nil {
		t.Fatalf("stat providers.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("providers.json perms = %o, want 0600 since it holds API keys", perm)
	}

	// Reopening must recover what was persisted.
	reopened, err := provider.NewProviderStore(tempDir, "")
	if err != nil {
		t.Fatalf("re-opening ProviderStore: %v", err)
	}
	cfg, ok := reopened.Get("p")
	if !ok || cfg.APIKey != "secret" {
		t.Fatalf("providers.json wasn't persisted/reloaded correctly: %+v, ok=%v", cfg, ok)
	}
}

func TestBuildProvider_UnknownKind(t *testing.T) {
	if _, err := provider.BuildProvider(provider.ProviderConfig{Name: "x", Kind: "not-a-real-kind"}, "", "/tmp"); err == nil {
		t.Fatal("expected an error building an unknown provider kind")
	}
}

func TestBuildProvider_CLIKinds(t *testing.T) {
	p, err := provider.BuildProvider(provider.ProviderConfig{Name: "x", Kind: provider.ProviderKindCLIClaude}, "opus", "/some/project")
	if err != nil {
		t.Fatalf("BuildProvider(cli_claude): %v", err)
	}
	cli, ok := p.(*provider.CLISubprocessProvider)
	if !ok {
		t.Fatalf("expected *CLISubprocessProvider, got %T", p)
	}
	if cli.Kind != provider.CLIKindClaude || cli.Model != "opus" || cli.Dir != "/some/project" {
		t.Fatalf("unexpected CLISubprocessProvider fields: %+v", cli)
	}
}

// A credential a client registered must not pick up the daemon's own keys or base URL.
func TestBuildProvider_IgnoresDaemonEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "host-key")
	t.Setenv("OPENAI_API_KEY", "host-key")
	t.Setenv("OPENAI_BASE_URL", "https://example.invalid/v1")
	for _, cfg := range []provider.ProviderConfig{
		{Name: "a", Kind: provider.ProviderKindAPIKey, HTTPProvider: "anthropic"},
		{Name: "o", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai"},
		{Name: "auto", Kind: provider.ProviderKindAPIKey},
	} {
		if p, err := provider.BuildProvider(cfg, "m", "/tmp"); err == nil {
			t.Errorf("%s: built %T from the daemon's environment", cfg.Name, p)
		}
	}
	p, err := provider.BuildProvider(provider.ProviderConfig{Name: "a", Kind: provider.ProviderKindAPIKey, HTTPProvider: "anthropic", APIKey: "own"}, "m", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := p.(*provider.AnthropicProvider); ok && a.BaseURL == "https://example.invalid/v1" {
		t.Fatal("OPENAI_BASE_URL leaked into an Anthropic provider")
	}
}

// ARC-5/PRV-12: an unparseable providers.json is set aside before the next
// save can overwrite it, so the API keys in it stay recoverable.
func TestProviderStore_CorruptFileSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := provider.NewProviderStore(dir, "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, APIKey: "k"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("expected one set-aside copy, got %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "{not json" {
		t.Fatalf("set-aside copy = %q, want the original bytes", b)
	}
}
