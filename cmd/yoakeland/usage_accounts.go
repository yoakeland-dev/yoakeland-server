package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// What an api_key provider's own account API says: OpenRouter's key usage
// and credits, DeepSeek's balance. The Anthropic and OpenAI usage APIs need
// an org admin key, which a chat key isn't, so they have nothing to offer.

var accountCache = struct {
	sync.Mutex
	m map[string]protocol.UsageAccount // provider name -> last answer
}{m: map[string]protocol.UsageAccount{}}

const accountTTL = 5 * time.Minute

var accountClient = &http.Client{Timeout: 5 * time.Second}

func usageAccounts(store *provider.ProviderStore) []protocol.UsageAccount {
	out := []protocol.UsageAccount{}
	if store == nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, listed := range store.List() {
		cfg, ok := store.Get(listed.Name)
		if !ok || cfg.Kind != provider.ProviderKindAPIKey || cfg.APIKey == "" {
			continue
		}
		fetch := accountFetcher(cfg)
		if fetch == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			accountCache.Lock()
			a, ok := accountCache.m[cfg.Name]
			accountCache.Unlock()
			if !ok || time.Since(a.FetchedAt) > accountTTL {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				a = fetch(ctx)
				cancel()
				a.Provider, a.FetchedAt = cfg.Name, time.Now().UTC()
				accountCache.Lock()
				accountCache.m[cfg.Name] = a
				accountCache.Unlock()
			}
			mu.Lock()
			out = append(out, a)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func accountFetcher(cfg provider.ProviderConfig) func(context.Context) protocol.UsageAccount {
	base := strings.ToLower(cfg.BaseURL)
	switch {
	case cfg.HTTPProvider == "openrouter" || strings.Contains(base, "openrouter.ai"):
		return func(ctx context.Context) protocol.UsageAccount {
			a := protocol.UsageAccount{Currency: "USD"}
			var key struct {
				Data struct {
					Limit          *float64 `json:"limit"`
					LimitRemaining *float64 `json:"limit_remaining"`
					Usage          float64  `json:"usage"`
					Daily          float64  `json:"usage_daily"`
					Weekly         float64  `json:"usage_weekly"`
					Monthly        float64  `json:"usage_monthly"`
				} `json:"data"`
			}
			if err := getJSON(ctx, "https://openrouter.ai/api/v1/key", cfg.APIKey, &key); err != nil {
				a.Error = err.Error()
				return a
			}
			d := key.Data
			a.Usage = &protocol.UsagePeriods{Day: d.Daily, Week: d.Weekly, Month: d.Monthly, Total: d.Usage}
			a.LimitUSD, a.LimitRemainingUSD = d.Limit, d.LimitRemaining
			var credits struct {
				Data struct {
					Total float64 `json:"total_credits"`
					Used  float64 `json:"total_usage"`
				} `json:"data"`
			}
			if getJSON(ctx, "https://openrouter.ai/api/v1/credits", cfg.APIKey, &credits) == nil {
				b := credits.Data.Total - credits.Data.Used
				a.Balance = &b
			}
			return a
		}
	case strings.Contains(base, "api.deepseek.com"):
		return func(ctx context.Context) protocol.UsageAccount {
			a := protocol.UsageAccount{}
			var bal struct {
				Infos []struct {
					Currency string `json:"currency"`
					Total    string `json:"total_balance"`
				} `json:"balance_infos"`
			}
			if err := getJSON(ctx, "https://api.deepseek.com/user/balance", cfg.APIKey, &bal); err != nil {
				a.Error = err.Error()
				return a
			}
			if len(bal.Infos) > 0 {
				if v, err := strconv.ParseFloat(bal.Infos[0].Total, 64); err == nil {
					a.Balance, a.Currency = &v, bal.Infos[0].Currency
				}
			}
			return a
		}
	}
	return nil
}

func getJSON(ctx context.Context, url, key string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := accountClient.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the provider")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
