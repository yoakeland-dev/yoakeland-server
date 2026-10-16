package provider

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"yoakeland/internal/fsutil"
)

// modelManifestURL is T3 Code's own model list (MIT-licensed, public repo, no
// auth needed). It's their internal implementation detail, not a published
// contract, so everything reading it (parseManifest) does so defensively.
const modelManifestURL = "https://raw.githubusercontent.com/pingdotgg/t3code/main/apps/server/src/provider/model-manifest.json"

// maxUpstreamBytes caps what a fetched model list or rate table may be: both
// are a few MB, and a runaway body must not be read whole into memory.
const maxUpstreamBytes = 8 << 20

// embeddedManifestFallback is a last-known-good snapshot baked into the
// binary, so a fresh install that has never successfully fetched still
// returns real model slugs instead of the old hand-maintained
// sonnet/opus/haiku stand-ins.
//
//go:embed manifest_fallback.json
var embeddedManifestFallback []byte

// manifestCacheFile is the on-disk shape at <stateDir>/model-manifest-cache.json:
// the raw upstream bytes plus the ETag that produced them, so a restart can
// resume conditional-GETing instead of re-fetching the full file.
type manifestCacheFile struct {
	ETag      string          `json:"etag"`
	FetchedAt time.Time       `json:"fetchedAt"`
	Raw       json.RawMessage `json:"raw"`
}

// ManifestCache serves cli_claude/cli_codex's model list from T3 Code's
// model-manifest.json. list_models never blocks on the network for this —
// Refresh runs only in the background (see StartDailyRefresh) and every read
// is served from whatever was last parsed successfully, all the way down to
// the embedded fallback above if this server has never fetched at all.
type ManifestCache struct {
	mu         sync.RWMutex
	cachePath  string // "" disables disk persistence (used by tests)
	url        string // overridable by tests; defaults to modelManifestURL
	httpClient *http.Client

	etag         string
	claudeModels []ModelInfo
	codexModels  []ModelInfo
	failStreak   int
}

// NewManifestCache loads <stateDir>/model-manifest-cache.json if present,
// falling back to the embedded snapshot otherwise (or on a corrupt cache
// file). It never fetches over the network — call Refresh or
// StartDailyRefresh for that.
func NewManifestCache(stateDir string) *ManifestCache {
	mc := &ManifestCache{url: modelManifestURL, httpClient: &http.Client{Timeout: 10 * time.Second}}
	if stateDir != "" {
		mc.cachePath = filepath.Join(stateDir, "model-manifest-cache.json")
	}

	raw := embeddedManifestFallback
	etag := ""
	if mc.cachePath != "" {
		if data, err := os.ReadFile(mc.cachePath); err == nil {
			var cached manifestCacheFile
			if json.Unmarshal(data, &cached) == nil && len(cached.Raw) > 0 {
				raw, etag = cached.Raw, cached.ETag
			}
		}
	}

	claude, codex, err := parseManifest(raw)
	if err != nil {
		log.Printf("model manifest: embedded/cached fallback failed to parse: %v", err)
	}
	mc.claudeModels, mc.codexModels, mc.etag = claude, codex, etag
	return mc
}

// ClaudeModels returns the cli_claude model list from the last successfully
// parsed manifest (disk cache, a live refresh, or the embedded fallback).
func (mc *ManifestCache) ClaudeModels() []ModelInfo {
	if mc == nil {
		return nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	out := make([]ModelInfo, len(mc.claudeModels))
	copy(out, mc.claudeModels)
	return out
}

// CodexModels is the cli_codex equivalent of ClaudeModels.
func (mc *ManifestCache) CodexModels() []ModelInfo {
	if mc == nil {
		return nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	out := make([]ModelInfo, len(mc.codexModels))
	copy(out, mc.codexModels)
	return out
}

// StartDailyRefresh fetches once immediately in the background (best-effort,
// never blocking the caller) and then again at a fixed 00:00 UTC every day
// after — a wall-clock trigger rather than a "24h since last boot" ticker, so
// a restart never drifts the schedule or redundantly re-fetches right after
// coming back up. Returns once ctx is done.
func (mc *ManifestCache) StartDailyRefresh(ctx context.Context) {
	go func() {
		_ = mc.Refresh()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(nextUTCMidnight(time.Now().UTC()))):
				_ = mc.Refresh()
			}
		}
	}()
}

func nextUTCMidnight(now time.Time) time.Time {
	y, m, d := now.Date()
	next := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Refresh does one conditional GET against modelManifestURL: If-None-Match
// with whatever ETag the last successful fetch returned, so a day where
// upstream hasn't changed costs a 304 rather than the full ~18KB body — good
// citizenship toward a repo yoakeland doesn't control. On any failure
// (network error, non-200/304 status, or a manifest that no longer parses)
// it logs and leaves the last-good cache serving exactly as it was; it never
// wipes state on a bad fetch.
func (mc *ManifestCache) Refresh() error {
	req, err := http.NewRequest(http.MethodGet, mc.url, nil)
	if err != nil {
		return err
	}
	mc.mu.RLock()
	etag := mc.etag
	mc.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		mc.recordFailure(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		mc.recordSuccess()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("model manifest fetch: unexpected status %d", resp.StatusCode)
		mc.recordFailure(err)
		return err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes))
	if err != nil {
		mc.recordFailure(err)
		return err
	}

	claude, codex, err := parseManifest(body)
	if err != nil {
		log.Printf("model manifest: fetched but failed to parse, keeping last-good cache: %v", err)
		mc.recordFailure(err)
		return err
	}

	newETag := resp.Header.Get("ETag")
	mc.mu.Lock()
	mc.claudeModels, mc.codexModels, mc.etag = claude, codex, newETag
	mc.mu.Unlock()
	mc.recordSuccess()

	if mc.cachePath != "" {
		data, err := json.Marshal(manifestCacheFile{ETag: newETag, FetchedAt: time.Now().UTC(), Raw: body})
		if err == nil {
			_ = fsutil.WriteFileAtomic(mc.cachePath, data, 0644)
		}
	}
	return nil
}

// recordFailure only warns once a failure has persisted across more than one
// scheduled (daily) attempt — a single miss is unremarkable network noise,
// but a streak means an operator should notice T3 may have moved/deleted the
// file for good.
func (mc *ManifestCache) recordFailure(err error) {
	mc.mu.Lock()
	mc.failStreak++
	streak := mc.failStreak
	mc.mu.Unlock()
	if streak >= 2 {
		log.Printf("WARNING: model manifest fetch failed %d times in a row (%v); serving the last-good cache", streak, err)
	}
}

func (mc *ManifestCache) recordSuccess() {
	mc.mu.Lock()
	mc.failStreak = 0
	mc.mu.Unlock()
}

// rawManifest is only the slice of T3's model-manifest.json shape yoakeland
// actually uses (model slugs plus their capability descriptors) — see
// manifest_fallback.json for the full upstream shape. Extra upstream fields
// are simply ignored by encoding/json, which is what keeps this defensive
// against upstream reshaping.
type rawManifest struct {
	CurrentModels struct {
		Codex       []string `json:"codex"`
		ClaudeAgent []string `json:"claudeAgent"`
	} `json:"currentModels"`
	Providers struct {
		ClaudeAgent struct {
			Profiles map[string]struct {
				Capabilities struct {
					OptionDescriptors []manifestOptionDescriptor `json:"optionDescriptors"`
				} `json:"capabilities"`
			} `json:"profiles"`
			Models []struct {
				Slug    string `json:"slug"`
				Name    string `json:"name"`
				Profile string `json:"profile"`
				Status  string `json:"status"`
			} `json:"models"`
		} `json:"claudeAgent"`
	} `json:"providers"`
}

// manifestOptionDescriptor is T3's per-model capability descriptor
// (currentModels.*.profile.capabilities.optionDescriptors[]). "effort",
// "fastMode", and "contextWindow" are the only ids yoakeland cross-walks
// into ModelInfo.Capabilities — "ultracode"/"ultrathink" are prompt-injection
// tricks (flagged via PromptInjectedValues), not real API parameters, so
// they're filtered out rather than modeled.
type manifestOptionDescriptor struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Options []struct {
		ID string `json:"id"`
	} `json:"options"`
	PromptInjectedValues []string `json:"promptInjectedValues"`
}

func parseManifest(raw []byte) (claude, codex []ModelInfo, err error) {
	var m rawManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("decoding model manifest: %w", err)
	}
	if len(m.CurrentModels.ClaudeAgent) == 0 && len(m.CurrentModels.Codex) == 0 {
		return nil, nil, fmt.Errorf("model manifest has no currentModels.claudeAgent/codex entries")
	}

	type modelMeta struct{ name, profile string }
	bySlug := make(map[string]modelMeta, len(m.Providers.ClaudeAgent.Models))
	for _, cm := range m.Providers.ClaudeAgent.Models {
		bySlug[cm.Slug] = modelMeta{cm.Name, cm.Profile}
	}

	buildInfo := func(slug string) ModelInfo {
		info := ModelInfo{ID: slug, DisplayName: slug}
		if meta, ok := bySlug[slug]; ok {
			if meta.name != "" {
				info.DisplayName = meta.name
			}
			if profile, ok := m.Providers.ClaudeAgent.Profiles[meta.profile]; ok {
				info.Capabilities = optionDescriptorCapabilities(profile.Capabilities.OptionDescriptors)
			}
		}
		return info
	}

	current := make(map[string]bool, len(m.CurrentModels.ClaudeAgent))
	for _, slug := range m.CurrentModels.ClaudeAgent {
		current[slug] = true
		claude = append(claude, buildInfo(slug))
	}
	// The manifest's own currentModels.claudeAgent list drops legacy models
	// silently; T3 still serves them (providers.claudeAgent.models entries
	// with status "legacy"), so surface those too instead of losing them —
	// appended after current ones, flagged via IsLegacy so a client can group
	// or de-prioritize them.
	for _, cm := range m.Providers.ClaudeAgent.Models {
		if cm.Status != "legacy" || current[cm.Slug] {
			continue
		}
		info := buildInfo(cm.Slug)
		info.IsLegacy = true
		claude = append(claude, info)
	}
	// codex has no per-model "providers.codex" block in T3's manifest today
	// (only claudeAgent/antigravity do), so codex models get the slug as
	// their own display name and no capabilities — still strictly better than
	// the old hand-maintained two-model stand-in list.
	for _, slug := range m.CurrentModels.Codex {
		codex = append(codex, ModelInfo{ID: slug, DisplayName: slug})
	}
	claude, codex = slugsOnly(claude), slugsOnly(codex)
	return claude, codex, nil
}

// slugPattern is the shape of a model id. Ids from upstream become --model
// arguments for the CLI agents, so a leading dash (a flag) or whitespace has no
// business there.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,127}$`)

// slugsOnly drops the models whose ids don't look like a model id.
func slugsOnly(models []ModelInfo) []ModelInfo {
	out := models[:0]
	for _, m := range models {
		if slugPattern.MatchString(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

func optionDescriptorCapabilities(descs []manifestOptionDescriptor) map[string]any {
	caps := map[string]any{}
	for _, d := range descs {
		injected := make(map[string]bool, len(d.PromptInjectedValues))
		for _, v := range d.PromptInjectedValues {
			injected[v] = true
		}
		switch d.ID {
		case "effort":
			var efforts []string
			for _, o := range d.Options {
				if injected[o.ID] {
					continue
				}
				efforts = append(efforts, o.ID)
			}
			if len(efforts) > 0 {
				caps["reasoningEfforts"] = efforts
			}
		case "fastMode":
			caps["fastMode"] = true
		case "contextWindow":
			var windows []string
			for _, o := range d.Options {
				if injected[o.ID] {
					continue
				}
				windows = append(windows, o.ID)
			}
			if len(windows) > 0 {
				caps["contextWindows"] = windows
			}
		}
	}
	if len(caps) == 0 {
		return nil
	}
	return caps
}
