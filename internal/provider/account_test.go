package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestNormalizeAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := ProviderConfig{Name: "w", Kind: ProviderKindCLIClaude, ConfigDir: "~/.claude_work"}
	if err := normalizeAccount(&cfg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude_work")
	if cfg.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, want)
	}
	if st, err := os.Stat(want); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("folder not created 0700: %v %v", st, err)
	}
	for _, bad := range []ProviderConfig{
		{Name: "a", Kind: ProviderKindCLIClaude, ConfigDir: "/etc"},
		{Name: "a", Kind: ProviderKindCLIClaude, ConfigDir: "~/../x"},
		{Name: "a", Kind: ProviderKindCLIClaude, ConfigDir: "~"},
		{Name: "a", Kind: ProviderKindCLIClaude, ConfigDir: "rel/dir"},
		{Name: "a", Kind: "cli_kimi", ConfigDir: "~/k"},
		{Name: "a", Kind: ProviderKindCLIClaude, Env: map[string]string{"A": "b"}},
		{Name: "a", Kind: ProviderKindCLIACP, Command: "x", Env: map[string]string{"LD_PRELOAD": "/x.so"}},
		{Name: "a", Kind: ProviderKindCLIACP},
		{Name: "a", Kind: ProviderKindAPIKey, ConfigDir: "~/x"},
	} {
		if err := normalizeAccount(&bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	ok := ProviderConfig{Name: "a", Kind: ProviderKindCLIACP, Command: "x", Env: map[string]string{"FOO": "1"}}
	if err := normalizeAccount(&ok); err != nil {
		t.Fatal(err)
	}
}

func TestAccountKeyAndEnv(t *testing.T) {
	a := ProviderConfig{Kind: ProviderKindCLIClaude}
	b := ProviderConfig{Kind: ProviderKindCLIClaude, ConfigDir: "/h/.c2"}
	c := ProviderConfig{Kind: ProviderKindCLIClaude, ConfigDir: "/h/.c2", Label: "other label"}
	if a.AccountKey() == b.AccountKey() || b.AccountKey() != c.AccountKey() {
		t.Fatalf("keys: %q %q %q", a.AccountKey(), b.AccountKey(), c.AccountKey())
	}
	if k := (ProviderConfig{Kind: "cli_kimi", ConfigDir: "/x"}).AccountKey(); k != "kimi" {
		t.Fatal("a one-login agent ignores a folder")
	}
	if got := procEnv(CLIKindCodex, "/h/.cx", nil); !slices.Equal(got, []string{"CODEX_HOME=/h/.cx"}) {
		t.Fatalf("env = %v", got)
	}
}

func TestEnvStarterReachesProcess(t *testing.T) {
	var seen []string
	base := func(ctx context.Context, _ string, _ []string, _ string) (*nativeProc, error) {
		seen = procEnvFrom(ctx)
		return nil, nil
	}
	_, _ = envStarter(base, []string{"CLAUDE_CONFIG_DIR=/x"})(context.Background(), "claude", nil, "")
	if !slices.Equal(seen, []string{"CLAUDE_CONFIG_DIR=/x"}) {
		t.Fatalf("env = %v", seen)
	}
}

func TestStoreKeepsEveryAccountOfAKind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := NewProviderStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"claude-work", "claude-personal"} {
		if err := s.Register(ProviderConfig{Name: n, Kind: ProviderKindCLIClaude, ConfigDir: "~/." + n}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.List()) != 2 {
		t.Fatalf("providers = %d, want 2", len(s.List()))
	}
}

func TestGeminiProviderMigratesToCustomACP(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(`{"providers":{"gemini-cli":{"name":"gemini-cli","kind":"cli_gemini"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewProviderStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := s.Get("gemini-cli")
	if cfg.Kind != ProviderKindCLIACP || cfg.Command != "gemini" || !slices.Equal(cfg.Args, []string{"--acp"}) {
		t.Fatalf("migrated = %+v", cfg)
	}
}
