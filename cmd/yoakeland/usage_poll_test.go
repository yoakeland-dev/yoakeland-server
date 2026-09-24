package main

import (
	"context"
	"testing"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

func usageTestHub(t *testing.T, probe func(context.Context, provider.CLIKind) (*protocol.ProviderRateLimits, error)) (*statusHub, *provider.ProviderStore) {
	t.Helper()
	store, err := provider.NewProviderStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	h := newStatusHub(NewPeerRegistry(), t.TempDir())
	h.probe = probe
	h.detect = func(provider.CLIKind) bool { return true }
	return h, store
}

// Disabling and re-enabling a provider within usageMinGap must bring its
// numbers straight back from the last read, not leave it empty until the
// next poll.
func TestUsageSurvivesReEnable(t *testing.T) {
	probes := 0
	h, store := usageTestHub(t, func(context.Context, provider.CLIKind) (*protocol.ProviderRateLimits, error) {
		probes++
		return &protocol.ProviderRateLimits{FiveHour: &protocol.RateLimitWindow{UsedPct: 22}}, nil
	})
	cfg := provider.ProviderConfig{Name: "claude-subscription", Kind: provider.ProviderKindCLIClaude}
	for i := 0; i < 3; i++ {
		if err := store.Register(cfg); err != nil {
			t.Fatal(err)
		}
		h.refreshUsage(context.Background(), store)
		if rl, ok := h.rateLimits()[cfg.Name]; !ok || rl.FiveHour == nil || rl.FiveHour.UsedPct != 22 {
			t.Fatalf("round %d: numbers missing after re-enable: %+v", i, h.rateLimits())
		}
		if err := store.Remove(cfg.Name); err != nil {
			t.Fatal(err)
		}
		h.ForgetRateLimits(cfg.Name)
	}
	if probes != 1 {
		t.Errorf("re-enabling within the gap should reuse the last read, got %d probes", probes)
	}
}

// A provider removed while its CLI's probe is in flight must not get its
// entry written back when the probe returns.
func TestUsageNotRestoredAfterRemoveMidProbe(t *testing.T) {
	var store *provider.ProviderStore
	h, s := usageTestHub(t, func(context.Context, provider.CLIKind) (*protocol.ProviderRateLimits, error) {
		_ = store.Remove("claude-subscription")
		return &protocol.ProviderRateLimits{FiveHour: &protocol.RateLimitWindow{UsedPct: 22}}, nil
	})
	store = s
	if err := store.Register(provider.ProviderConfig{Name: "claude-subscription", Kind: provider.ProviderKindCLIClaude}); err != nil {
		t.Fatal(err)
	}
	h.refreshUsage(context.Background(), store)
	if _, ok := h.rateLimits()["claude-subscription"]; ok {
		t.Fatalf("removed provider came back: %+v", h.rateLimits())
	}
}
