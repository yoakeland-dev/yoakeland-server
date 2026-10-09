package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Which models a CLI agent offers is the agent's to say, and depends on the
// account: Claude Code answers an initialize control request with the list
// /model shows, Codex answers model/list. Both are asked of a throwaway
// process, cached, and the manifest (manifest.go) is what's used when the
// agent can't be asked, and what adds what the agent doesn't report (Claude
// Code's ultracode setting, context windows).

// startDiscovery starts the throwaway processes; tests replace it.
var startDiscovery procStarter = startProc

const (
	discoveryTTL     = 30 * time.Minute
	discoveryTimeout = 25 * time.Second
	discoveryBackoff = 2 * time.Minute // before asking again after a failure
)

type discoveryEntry struct {
	models   []ModelInfo
	at       time.Time
	failedAt time.Time
	loading  bool
}

type discoveryCache struct {
	mu      sync.Mutex
	entries map[CLIKind]*discoveryEntry
}

var discovered = &discoveryCache{entries: map[CLIKind]*discoveryEntry{}}

// get returns kind's models: fresh ones from the cache, stale ones straight
// away while a refresh runs behind them, or — the first time — the answer of
// the agent itself. fallback is what a failed or missing answer is replaced by.
func (c *discoveryCache) get(kind CLIKind, fetch func(context.Context) ([]ModelInfo, error), fallback func() []ModelInfo) []ModelInfo {
	c.mu.Lock()
	e := c.entries[kind]
	if e == nil {
		e = &discoveryEntry{}
		c.entries[kind] = e
	}
	if len(e.models) > 0 && time.Since(e.at) < discoveryTTL {
		defer c.mu.Unlock()
		return e.models
	}
	if len(e.models) == 0 && !e.failedAt.IsZero() && time.Since(e.failedAt) < discoveryBackoff {
		c.mu.Unlock()
		return fallback()
	}
	refresh := func() ([]ModelInfo, error) {
		ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
		defer cancel()
		models, err := fetch(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		e.loading = false
		if err != nil || len(models) == 0 {
			e.failedAt = time.Now()
			if err != nil {
				log.Printf("model discovery for %s failed, using the manifest: %v", kind, err)
			}
			return nil, err
		}
		e.models, e.at, e.failedAt = models, time.Now(), time.Time{}
		return models, nil
	}
	if len(e.models) > 0 { // stale: answer now, refresh behind it
		stale := e.models
		if !e.loading {
			e.loading = true
			go func() { _, _ = refresh() }()
		}
		c.mu.Unlock()
		return stale
	}
	e.loading = true
	c.mu.Unlock()
	if models, _ := refresh(); len(models) > 0 {
		return models
	}
	return fallback()
}

// discoverProc starts a throwaway CLI process for one question.
func discoverProc(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
	// The process is bound to ctx, so it dies with the timeout however the call ends.
	return startDiscovery(ctx, name, args, dir)
}

type claudeModelInfo struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
	SupportsFastMode      bool     `json:"supportsFastMode"`
	SupportsAutoMode      bool     `json:"supportsAutoMode"`
}

var errClaudeLoggedOut = errors.New("claude is not logged in (run `claude auth login`)")

// discoverClaudeModels asks a throwaway `claude` for the models this account
// can use, with what each supports.
func discoverClaudeModels(ctx context.Context, dir string) ([]ModelInfo, error) {
	proc, err := discoverProc(ctx, "claude", []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = proc.stdin.Close() }()
	var wmu sync.Mutex
	if err := writeJSONLine(proc.stdin, &wmu, map[string]any{"type": "control_request", "request_id": "yk-models", "request": map[string]any{"subtype": "initialize"}}); err != nil {
		return nil, err
	}
	type result struct {
		models []claudeModelInfo
		err    error
	}
	done := make(chan result, 1)
	go func() {
		for proc.lines.Scan() {
			var ln struct {
				Type     string `json:"type"`
				Response struct {
					Subtype   string `json:"subtype"`
					RequestID string `json:"request_id"`
					Error     string `json:"error"`
					Response  struct {
						Models  []claudeModelInfo `json:"models"`
						Account struct {
							TokenSource string `json:"tokenSource"`
						} `json:"account"`
					} `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(proc.lines.Bytes(), &ln) != nil || ln.Type != "control_response" || ln.Response.RequestID != "yk-models" {
				continue
			}
			if ln.Response.Subtype != "success" {
				done <- result{err: fmt.Errorf("claude initialize: %s", ln.Response.Error)}
				return
			}
			// Logged out, Claude Code still answers, but with a generic list
			// (no legacy models, no account's own): not worth caching.
			if ln.Response.Response.Account.TokenSource == "none" {
				done <- result{err: errClaudeLoggedOut}
				return
			}
			done <- result{models: ln.Response.Response.Models}
			return
		}
		done <- result{err: fmt.Errorf("claude exited before listing its models")}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return claudeModelsFrom(r.models), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func claudeModelsFrom(in []claudeModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(in))
	for _, m := range in {
		if m.Value == "" {
			continue
		}
		caps := map[string]any{}
		if len(m.SupportedEffortLevels) > 0 {
			caps["reasoningEfforts"] = m.SupportedEffortLevels
		}
		if m.SupportsFastMode {
			caps["fastMode"] = true
		}
		if m.SupportsAutoMode {
			caps["autoMode"] = true
		}
		if m.Description != "" {
			caps["description"] = m.Description
		}
		if m.ResolvedModel != "" {
			caps["resolvedModel"] = m.ResolvedModel
		}
		name := m.DisplayName
		if name == "" {
			name = m.Value
		}
		// Claude Code says "Opus 5.5"; "default" is its own label for whatever it picks.
		if m.Value != "default" && !strings.HasPrefix(name, "Claude") {
			name = "Claude " + name
		}
		info := ModelInfo{ID: m.Value, DisplayName: name}
		if len(caps) > 0 {
			info.Capabilities = caps
		}
		out = append(out, info)
	}
	return out
}

// withManifest adds what Claude Code doesn't report about a model but the
// manifest knows: the ultracode setting on top of xhigh, context windows, and
// which models are legacy.
// A model is matched by its resolved name, the longest manifest slug it starts with.
func withManifest(models, manifest []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, len(models))
	for i, m := range models {
		out[i] = m
		name, _ := m.Capabilities["resolvedModel"].(string)
		if name == "" {
			name = m.ID
		}
		var best *ModelInfo
		for j := range manifest {
			slug := manifest[j].ID
			if (name == slug || strings.HasPrefix(name, slug+"-")) && (best == nil || len(slug) > len(best.ID)) {
				best = &manifest[j]
			}
		}
		if best == nil {
			continue
		}
		caps := make(map[string]any, len(m.Capabilities)+2)
		for k, v := range m.Capabilities {
			caps[k] = v
		}
		if efforts, ok := caps["reasoningEfforts"].([]string); ok && contains(efforts, "xhigh") {
			if theirs, _ := best.Capabilities["reasoningEfforts"].([]string); contains(theirs, "ultracode") {
				caps["reasoningEfforts"] = append(append([]string(nil), efforts...), "ultracode")
			}
		}
		if w, ok := best.Capabilities["contextWindows"]; ok {
			caps["contextWindows"] = w
		}
		out[i].Capabilities = caps
		out[i].IsLegacy = best.IsLegacy // the picker's divider
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ClaudeModelsDiscovered is Claude Code's own model list (cached), enriched
// from and falling back to the manifest.
func ClaudeModelsDiscovered(manifest *ManifestCache, dir string) []ModelInfo {
	fallback := func() []ModelInfo { return manifest.ClaudeModels() }
	return discovered.get(CLIKindClaude, func(ctx context.Context) ([]ModelInfo, error) {
		models, err := discoverClaudeModels(ctx, dir)
		if err != nil {
			return nil, err
		}
		return withManifest(models, manifest.ClaudeModels()), nil
	}, fallback)
}

type codexModelInfo struct {
	ID                        string `json:"id"`
	Model                     string `json:"model"`
	DisplayName               string `json:"displayName"`
	Description               string `json:"description"`
	Hidden                    bool   `json:"hidden"`
	IsDefault                 bool   `json:"isDefault"`
	DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
	ServiceTiers []struct {
		ID string `json:"id"`
	} `json:"serviceTiers"`
}

// discoverCodexModels pages model/list of a throwaway app-server.
func discoverCodexModels(ctx context.Context, dir string) ([]ModelInfo, error) {
	proc, err := discoverProc(ctx, "codex", []string{"app-server"}, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = proc.stdin.Close() }()
	var conn *rpcConn
	// Nothing here is a turn, so nothing has a reason to ask.
	conn = newRPCConn(proc, func(string, json.RawMessage) {}, func(id json.RawMessage, method string, _ json.RawMessage) {
		conn.respondError(id, -32601, "not supported")
	})
	return listCodexModels(ctx, conn)
}

func listCodexModels(ctx context.Context, conn *rpcConn) ([]ModelInfo, error) {
	if _, err := conn.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "yoakeland", "version": "1"}, "capabilities": nil}); err != nil {
		return nil, err
	}
	conn.notify("initialized", nil)
	var models []codexModelInfo
	cursor := ""
	for page := 0; page < 32; page++ {
		params := map[string]any{"limit": 50}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := conn.call(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		var r struct {
			Data       []codexModelInfo `json:"data"`
			NextCursor *string          `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		models = append(models, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		cursor = *r.NextCursor
	}
	return codexModelsFrom(models), nil
}

func codexModelsFrom(in []codexModelInfo) []ModelInfo {
	// The account's default first, then in the order Codex lists them.
	sort.SliceStable(in, func(i, j int) bool { return in[i].IsDefault && !in[j].IsDefault })
	var out []ModelInfo
	for _, m := range in {
		id := m.Model
		if id == "" {
			id = m.ID
		}
		if id == "" || m.Hidden {
			continue
		}
		caps := map[string]any{}
		var efforts []string
		for _, e := range m.SupportedReasoningEfforts {
			efforts = append(efforts, e.ReasoningEffort)
		}
		if len(efforts) > 0 {
			caps["reasoningEfforts"] = efforts
		}
		for _, t := range m.ServiceTiers {
			if t.ID == "fast" {
				caps["fastMode"] = true
			}
		}
		if m.Description != "" {
			caps["description"] = m.Description
		}
		name := m.DisplayName
		if name == "" {
			name = id
		}
		info := ModelInfo{ID: id, DisplayName: codexDisplayName(name)}
		if len(caps) > 0 {
			info.Capabilities = caps
		}
		out = append(out, info)
	}
	return out
}

var codexNameDashes = regexp.MustCompile(`^(GPT-\d[\d.]*)-(.+)$`)

// codexDisplayName reads "GPT-6-Luna" as "GPT-6 Luna": the dash belongs to the
// version, not to what follows it.
func codexDisplayName(name string) string {
	m := codexNameDashes.FindStringSubmatch(name)
	if m == nil {
		return name
	}
	return m[1] + " " + strings.ReplaceAll(m[2], "-", " ")
}

// CodexModelsDiscovered is Codex's own model list for this account (cached),
// falling back to the manifest.
func CodexModelsDiscovered(manifest *ManifestCache, dir string) []ModelInfo {
	return discovered.get(CLIKindCodex, func(ctx context.Context) ([]ModelInfo, error) {
		return discoverCodexModels(ctx, dir)
	}, func() []ModelInfo { return manifest.CodexModels() })
}
