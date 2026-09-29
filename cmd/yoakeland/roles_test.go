package main

import (
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

func TestBuildRoles(t *testing.T) {
	store, err := provider.NewProviderStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Register(provider.ProviderConfig{Name: "anth", Kind: provider.ProviderKindAPIKey, HTTPProvider: "anthropic", APIKey: "k"})
	_ = store.Register(provider.ProviderConfig{Name: "or", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openrouter", APIKey: "k"})
	_ = store.Register(provider.ProviderConfig{Name: "cc", Kind: provider.ProviderKind("cli_claude")})

	live, err := buildRoles(store, map[string]protocol.ModelRole{
		"explore": {ProviderName: "or", Model: "some/cheap-model"},
		"advisor": {ProviderName: "anth", Model: "claude-opus-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := live[agent.RoleExplore]; b.Provider == nil || b.Model != "some/cheap-model" {
		t.Fatalf("explore binding = %+v", b)
	}

	for table, want := range map[string]map[string]protocol.ModelRole{
		"unknown model role": {"planner": {ProviderName: "anth"}},
		"unknown provider":   {"plan": {ProviderName: "nope"}},
		"is a CLI agent":     {"plan": {ProviderName: "cc"}},
		"needs an Anthropic": {"advisor": {ProviderName: "or", Model: "anthropic/claude-opus-5"}},
	} {
		if _, err := buildRoles(store, want); err == nil || !strings.Contains(err.Error(), table) {
			t.Errorf("want error containing %q, got %v", table, err)
		}
	}
}
