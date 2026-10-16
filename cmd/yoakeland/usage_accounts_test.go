package main

import (
	"testing"

	"yoakeland/internal/provider"
)

// An API key is sent to a vendor's account API only when the provider really
// talks to that vendor: a proxy, or a lookalike host, must never get it.
func TestAccountFetcherOnlyForVendorHosts(t *testing.T) {
	cases := []struct {
		name string
		cfg  provider.ProviderConfig
		want bool
	}{
		{"openrouter default", provider.ProviderConfig{HTTPProvider: "openrouter"}, true},
		{"openrouter own host", provider.ProviderConfig{HTTPProvider: "openrouter", BaseURL: "https://openrouter.ai/api/v1/chat/completions"}, true},
		{"openrouter proxy", provider.ProviderConfig{HTTPProvider: "openrouter", BaseURL: "https://proxy.example/v1"}, false},
		{"lookalike path", provider.ProviderConfig{HTTPProvider: "openai", BaseURL: "https://evil.example/openrouter.ai"}, false},
		{"deepseek own host", provider.ProviderConfig{BaseURL: "https://api.deepseek.com/v1"}, true},
		{"deepseek lookalike", provider.ProviderConfig{BaseURL: "https://api.deepseek.com.evil.example/v1"}, false},
	}
	for _, c := range cases {
		if got := accountFetcher(c.cfg) != nil; got != c.want {
			t.Errorf("%s: fetcher present = %v, want %v", c.name, got, c.want)
		}
	}
}
