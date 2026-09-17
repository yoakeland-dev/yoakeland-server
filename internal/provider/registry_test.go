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

// TestProviderStore_CLIRegistrationIsSingletonPerKind covers item 6 of the
// OpenRouter checklist: since there's only ever one `claude`/`codex` CLI on
// the machine, re-registering a cli_claude/cli_codex provider under a new
// Name must replace the old entry of that same kind rather than leaving a
// stale duplicate slot behind — while still honoring whatever Name the
// caller most recently chose for lookup.
func TestProviderStore_CLIRegistrationIsSingletonPerKind(t *testing.T) {
	tempDir := t.TempDir()
	store, err := provider.NewProviderStore(tempDir, "")
	if err != nil {
		t.Fatalf("NewProviderStore: %v", err)
	}

	if err := store.Register(provider.ProviderConfig{Name: "my-claude", Kind: provider.ProviderKindCLIClaude}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.Register(provider.ProviderConfig{Name: "renamed-claude", Kind: provider.ProviderKindCLIClaude}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A different kind (cli_codex) must be left alone by cli_claude churn.
	if err := store.Register(provider.ProviderConfig{Name: "my-codex", Kind: provider.ProviderKindCLICodex}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, ok := store.Get("my-claude"); ok {
		t.Error("expected the old cli_claude entry to be replaced, not kept alongside the new one")
	}
	if _, ok := store.Get("renamed-claude"); !ok {
		t.Error("expected the most recently registered cli_claude entry to be findable by its name")
	}
	if _, ok := store.Get("my-codex"); !ok {
		t.Error("expected the unrelated cli_codex entry to survive cli_claude re-registration")
	}

	all := store.List()
	cliCount := 0
	for _, c := range all {
		if c.Kind == provider.ProviderKindCLIClaude {
			cliCount++
		}
	}
	if cliCount != 1 {
		t.Errorf("expected exactly one cli_claude provider after re-registration, got %d", cliCount)
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
