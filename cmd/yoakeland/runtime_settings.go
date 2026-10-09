package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"yoakeland/internal/protocol"
)

// Server-wide tunables that used to be launch flags. Everything here is
// persisted in server.json (see ServerSettings), pushed to each peer on
// connect, and changed at runtime over the wire; a launch flag may only seed
// a value the first time (see seedFromFlags in main.go).

// runtimeState is the on-disk shape of protocol.RuntimeSettings. Decoding
// starts from the defaults, so a key missing from an older file gets its
// default rather than a zero value (which matters for the settings whose
// default is not zero).
type runtimeState struct {
	protocol.RuntimeSettings
}

func (r *runtimeState) UnmarshalJSON(b []byte) error {
	r.RuntimeSettings = defaultRuntimeSettings()
	type plain protocol.RuntimeSettings
	return json.Unmarshal(b, (*plain)(&r.RuntimeSettings))
}

func defaultRuntimeSettings() protocol.RuntimeSettings {
	return normalizeRuntimeSettings(protocol.RuntimeSettings{BrowserBlockCrossOrigin: true})
}

func clampInt(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}

// normalizeRuntimeSettings replaces 0 with a setting's default and clamps
// everything into its valid range, so what is stored, broadcast and read by
// consumers is always usable.
func normalizeRuntimeSettings(rs protocol.RuntimeSettings) protocol.RuntimeSettings {
	rs.MaxRoundsPerTurn = min(max(rs.MaxRoundsPerTurn, 0), 100000)
	if rs.MaxSessionCostUSD < 0 || math.IsNaN(rs.MaxSessionCostUSD) || math.IsInf(rs.MaxSessionCostUSD, 0) {
		rs.MaxSessionCostUSD = 0
	}
	rs.ExecTimeoutSec = clampInt(rs.ExecTimeoutSec, 60, 1, 3600)
	rs.ExecOutputMaxBytes = clampInt(rs.ExecOutputMaxBytes, 1<<20, 4096, 1<<28)
	rs.TerminalMemoryMaxMB = clampInt(rs.TerminalMemoryMaxMB, 2048, 64, 1<<20)
	rs.TerminalPidsMax = clampInt(rs.TerminalPidsMax, 512, 16, 1<<20)
	rs.TerminalReadMaxChars = clampInt(rs.TerminalReadMaxChars, 12000, 500, 1_000_000)
	rs.TerminalHistoryKB = clampInt(rs.TerminalHistoryKB, 256, 16, 65536)
	rs.PoolIdleMinutes = clampInt(rs.PoolIdleMinutes, 30, 1, 10080)
	rs.PoolMaxLive = min(max(rs.PoolMaxLive, 0), 1000)
	rs.PricingRefreshHours = clampInt(rs.PricingRefreshHours, 24, 1, 720)
	return rs
}

// mergeRuntimePatch overlays the keys present in patch onto cur, rejecting
// unknown keys and wrongly-typed values, then normalizes the result.
func mergeRuntimePatch(cur protocol.RuntimeSettings, patch map[string]json.RawMessage) (protocol.RuntimeSettings, error) {
	raw, err := json.Marshal(patch)
	if err != nil {
		return cur, err
	}
	next := cur
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return cur, fmt.Errorf("invalid runtime settings: %v", err)
	}
	return normalizeRuntimeSettings(next), nil
}

// runtimeHook applies runtime settings to the live parts of the server that
// consume them. It gets the full settings, is idempotent, and runs once at
// registration and again after every change that alters something.
type runtimeHook func(protocol.RuntimeSettings)

// Runtime returns the current runtime settings (defaults if never set).
func (s *ServerSettings) Runtime() protocol.RuntimeSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimeLocked()
}

func (s *ServerSettings) runtimeLocked() protocol.RuntimeSettings {
	if s.RuntimeState == nil {
		return defaultRuntimeSettings()
	}
	return s.RuntimeState.RuntimeSettings
}

// OnRuntimeChange registers hook and runs it once with the current settings,
// so startup and later changes share one path. This is where a new setting's
// consumer gets attached. Consumers: maxRoundsPerTurn, maxSessionCostUsd,
// disableBashWhenUnsandboxed and browserBlockCrossOrigin (the project registry,
// ApplyRuntime); the sandbox, terminal, pool and pricing fields
// (applyProcessLimits in runtime_apply.go).
func (s *ServerSettings) OnRuntimeChange(hook runtimeHook) {
	s.mu.Lock()
	s.runtimeHooks = append(s.runtimeHooks, hook)
	cur := s.runtimeLocked()
	s.mu.Unlock()
	hook(cur)
}

// UpdateRuntime applies a partial update, persists it, and runs the hooks
// (outside the lock) if anything changed. It returns the settings now in
// effect.
func (s *ServerSettings) UpdateRuntime(patch map[string]json.RawMessage) (protocol.RuntimeSettings, error) {
	s.mu.Lock()
	cur := s.runtimeLocked()
	next, err := mergeRuntimePatch(cur, patch)
	if err != nil {
		s.mu.Unlock()
		return cur, err
	}
	if next == cur && s.RuntimeState != nil {
		s.mu.Unlock()
		return cur, nil
	}
	prev := s.RuntimeState
	s.RuntimeState = &runtimeState{RuntimeSettings: next}
	if err := s.save(); err != nil {
		s.RuntimeState = prev
		s.mu.Unlock()
		return cur, err
	}
	hooks := append([]runtimeHook(nil), s.runtimeHooks...)
	s.mu.Unlock()
	for _, h := range hooks {
		h(next)
	}
	return next, nil
}

// SeedRuntime applies edit once, when no runtime settings were ever
// persisted: it is how a deprecated launch flag hands its value over. Once
// anything is stored, a flag never overrides it. Reports whether it seeded.
func (s *ServerSettings) SeedRuntime(edit func(*protocol.RuntimeSettings)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RuntimeState != nil {
		return false
	}
	rs := defaultRuntimeSettings()
	edit(&rs)
	s.RuntimeState = &runtimeState{RuntimeSettings: normalizeRuntimeSettings(rs)}
	if err := s.save(); err != nil {
		s.RuntimeState = nil
		logWarn("server settings: could not persist seeded runtime settings: %v", err)
		return false
	}
	return true
}

// RemoteSettings is the optional TCP listener's persisted configuration: the
// replacement for the -listen host:port, -tls-cert, -tls-key and -web-root
// launch flags. Enabled is what set_remote_access toggles; it is remembered
// so the listener comes back after a restart.
type RemoteSettings struct {
	Enabled bool   `json:"enabled"`
	Addr    string `json:"addr,omitempty"`
	TLSCert string `json:"tlsCert,omitempty"`
	TLSKey  string `json:"tlsKey,omitempty"`
	WebRoot string `json:"webRoot,omitempty"`
}

// Remote returns the persisted remote-access configuration (zero if never set).
func (s *ServerSettings) Remote() RemoteSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RemoteState == nil {
		return RemoteSettings{}
	}
	return *s.RemoteState
}

// UpdateRemote edits and persists the remote configuration.
func (s *ServerSettings) UpdateRemote(edit func(*RemoteSettings)) (RemoteSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next RemoteSettings
	if s.RemoteState != nil {
		next = *s.RemoteState
	}
	edit(&next)
	if (next.TLSCert == "") != (next.TLSKey == "") {
		return next, fmt.Errorf("tlsCert and tlsKey must be set together")
	}
	prev := s.RemoteState
	s.RemoteState = &next
	if err := s.save(); err != nil {
		s.RemoteState = prev
		return next, err
	}
	return next, nil
}

// SeedRemote is SeedRuntime's counterpart for the remote listener flags.
func (s *ServerSettings) SeedRemote(rs RemoteSettings) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RemoteState != nil {
		return false
	}
	s.RemoteState = &rs
	if err := s.save(); err != nil {
		s.RemoteState = nil
		logWarn("server settings: could not persist seeded remote settings: %v", err)
		return false
	}
	return true
}

// DefaultProviderSetting names the registered provider (and model) a session
// uses until it calls set_provider. Empty Name means none.
type DefaultProviderSetting struct {
	Name  string `json:"name,omitempty"`
	Model string `json:"model,omitempty"`
}

func (s *ServerSettings) DefaultProvider() DefaultProviderSetting {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DefaultProviderState == nil {
		return DefaultProviderSetting{}
	}
	return *s.DefaultProviderState
}

func (s *ServerSettings) SetDefaultProvider(name, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.DefaultProviderState
	// A pointer to an empty value, not nil, when cleared: "cleared on
	// purpose" must not look like "never set" to a launch-flag seed.
	s.DefaultProviderState = &DefaultProviderSetting{Name: strings.TrimSpace(name), Model: model}
	if err := s.save(); err != nil {
		s.DefaultProviderState = prev
		return err
	}
	return nil
}

// DefaultProviderUnset reports that no default provider was ever stored (not
// even a cleared one), so a launch flag may seed it.
func (s *ServerSettings) DefaultProviderUnset() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DefaultProviderState == nil
}

// SeedDefaultProvider is SeedRuntime's counterpart for the provider flags.
func (s *ServerSettings) SeedDefaultProvider(name, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DefaultProviderState != nil {
		return false
	}
	s.DefaultProviderState = &DefaultProviderSetting{Name: name, Model: model}
	if err := s.save(); err != nil {
		s.DefaultProviderState = nil
		logWarn("server settings: could not persist seeded default provider: %v", err)
		return false
	}
	return true
}
