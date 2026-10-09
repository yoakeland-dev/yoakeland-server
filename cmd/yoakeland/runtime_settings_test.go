package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sandbox"
)

func patch(t *testing.T, kv string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(kv), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeRuntimeSettings_DefaultsAndClamps(t *testing.T) {
	d := defaultRuntimeSettings()
	want := protocol.RuntimeSettings{
		ExecTimeoutSec: 60, ExecOutputMaxBytes: 1 << 20, TerminalMemoryMaxMB: 2048, TerminalPidsMax: 512,
		TerminalReadMaxChars: 12000, TerminalHistoryKB: 256, PoolIdleMinutes: 30, PricingRefreshHours: 24,
		BrowserBlockCrossOrigin: true,
	}
	if d != want {
		t.Fatalf("defaults = %+v, want %+v", d, want)
	}

	got := normalizeRuntimeSettings(protocol.RuntimeSettings{
		MaxRoundsPerTurn: -4, MaxSessionCostUSD: -1, ExecTimeoutSec: 99999, TerminalMemoryMaxMB: 3, TerminalPidsMax: 1,
		PoolIdleMinutes: -5, PoolMaxLive: -1, ExecOutputMaxBytes: 10,
	})
	if got.MaxRoundsPerTurn != 0 || got.MaxSessionCostUSD != 0 || got.ExecTimeoutSec != 3600 || got.TerminalMemoryMaxMB != 64 ||
		got.TerminalPidsMax != 16 || got.PoolIdleMinutes != 1 || got.PoolMaxLive != 0 || got.ExecOutputMaxBytes != 4096 {
		t.Fatalf("clamped = %+v", got)
	}
}

func TestMergeRuntimePatch_PartialAndStrict(t *testing.T) {
	cur := defaultRuntimeSettings()
	got, err := mergeRuntimePatch(cur, patch(t, `{"maxRoundsPerTurn":25,"disableBashWhenUnsandboxed":true,"browserBlockCrossOrigin":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRoundsPerTurn != 25 || !got.DisableBashWhenUnsandboxed || got.BrowserBlockCrossOrigin {
		t.Fatalf("patched = %+v", got)
	}
	if got.ExecTimeoutSec != 60 || got.TerminalPidsMax != 512 { // untouched keys keep their value
		t.Fatalf("a partial update changed other keys: %+v", got)
	}
	if _, err := mergeRuntimePatch(cur, patch(t, `{"nope":1}`)); err == nil {
		t.Fatal("an unknown key must be rejected")
	}
	if _, err := mergeRuntimePatch(cur, patch(t, `{"maxRoundsPerTurn":"lots"}`)); err == nil {
		t.Fatal("a wrongly typed value must be rejected")
	}
	// 0 puts a defaulted setting back to its default.
	back, _ := mergeRuntimePatch(got, patch(t, `{"execTimeoutSec":0}`))
	if back.ExecTimeoutSec != 60 {
		t.Fatalf("execTimeoutSec 0 = %d, want the default", back.ExecTimeoutSec)
	}
}

func TestServerSettings_RuntimePersistsAndFiresHooks(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []protocol.RuntimeSettings
	s.OnRuntimeChange(func(rs protocol.RuntimeSettings) { seen = append(seen, rs) })
	if len(seen) != 1 || seen[0] != defaultRuntimeSettings() {
		t.Fatalf("a hook must run once at registration with the current settings, got %+v", seen)
	}

	rs, err := s.UpdateRuntime(patch(t, `{"maxSessionCostUsd":2.5,"poolMaxLive":3}`))
	if err != nil || rs.MaxSessionCostUSD != 2.5 || rs.PoolMaxLive != 3 {
		t.Fatalf("update = %+v, %v", rs, err)
	}
	if len(seen) != 2 || seen[1].MaxSessionCostUSD != 2.5 {
		t.Fatalf("hook not called with the new settings: %+v", seen)
	}
	if _, err := s.UpdateRuntime(patch(t, `{"maxSessionCostUsd":2.5}`)); err != nil || len(seen) != 2 {
		t.Fatalf("a no-op update fired the hooks again (%d calls, err %v)", len(seen), err)
	}
	if _, err := s.UpdateRuntime(patch(t, `{"bogus":true}`)); err == nil || s.Runtime().MaxSessionCostUSD != 2.5 {
		t.Fatalf("a rejected patch must change nothing (err %v)", err)
	}

	reopened, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Runtime(); got != rs {
		t.Fatalf("after reopen = %+v, want %+v", got, rs)
	}
}

// An older server.json holding only some runtime keys gets the defaults for
// the rest, including the ones whose default is not zero.
func TestServerSettings_RuntimeMissingKeysTakeDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.json"), []byte(`{"runtime":{"maxRoundsPerTurn":5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Runtime()
	if got.MaxRoundsPerTurn != 5 || !got.BrowserBlockCrossOrigin || got.ExecTimeoutSec != 60 {
		t.Fatalf("runtime = %+v", got)
	}
}

func decodeEvents(t *testing.T, raw string) map[protocol.EventType][]json.RawMessage {
	t.Helper()
	out := map[protocol.EventType][]json.RawMessage{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("bad output line %q: %v", line, err)
		}
		out[env.Type] = append(out[env.Type], env.Data)
	}
	return out
}

func TestSetRuntimeSettings_AppliesBroadcastsAndClamps(t *testing.T) {
	deps := newTestDeps(t)
	deps.settings.OnRuntimeChange(deps.projects.ApplyRuntime)

	bystander := &bytes.Buffer{}
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), bystander), &fakeCloser{}, "unix", peerIdentity{Name: "other", DeviceType: "desktop"})

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventSetRuntimeSettings,
		map[string]any{"maxRoundsPerTurn": 40, "maxSessionCostUsd": 7.5, "execTimeoutSec": 100000}))
	var reply protocol.RuntimeSettings
	found := false
	for _, e := range envs {
		if e.Type == protocol.EventRuntimeSettings {
			found = true
			if err := json.Unmarshal(e.Data, &reply); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found || reply.MaxRoundsPerTurn != 40 || reply.MaxSessionCostUSD != 7.5 || reply.ExecTimeoutSec != 3600 {
		t.Fatalf("reply = %+v (found %v); the clamped value must be echoed", reply, found)
	}
	if rounds, cost := deps.projects.limits.Get(); rounds != 40 || cost != 7.5 {
		t.Fatalf("limits holder = %d/%v, want 40/7.5", rounds, cost)
	}
	if !strings.Contains(bystander.String(), `"type":"runtime_settings"`) {
		t.Fatalf("other peers were not told: %s", bystander.String())
	}

	envs = runDispatch(t, deps, envelopeJSON(t, protocol.EventSetRuntimeSettings, map[string]any{"mystery": 1}))
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("an unknown setting must be reported as an error, got %+v", envs)
	}
}

func TestSendRuntimeState_OnConnect(t *testing.T) {
	deps := newTestDeps(t)
	if _, err := deps.settings.UpdateRemote(func(r *RemoteSettings) { r.Enabled, r.Addr, r.WebRoot = true, ":9000", "/srv/web" }); err != nil {
		t.Fatal(err)
	}
	var local, remote bytes.Buffer
	sendRuntimeState(protocol.NewIPCStream(nil, &local), deps, "unix")
	sendRuntimeState(protocol.NewIPCStream(nil, &remote), deps, "tcp")
	le, re := decodeEvents(t, local.String()), decodeEvents(t, remote.String())
	for _, typ := range []protocol.EventType{protocol.EventRuntimeSettings, protocol.EventDefaultProviderSet, protocol.EventRemoteAccessSet} {
		if len(le[typ]) != 1 || len(re[typ]) != 1 {
			t.Fatalf("connect state is missing %s", typ)
		}
	}
	var lr, rr protocol.RemoteAccessSetData
	_ = json.Unmarshal(le[protocol.EventRemoteAccessSet][0], &lr)
	_ = json.Unmarshal(re[protocol.EventRemoteAccessSet][0], &rr)
	if !lr.Enabled || lr.Addr != ":9000" || lr.WebRoot != "/srv/web" {
		t.Fatalf("local peer sees %+v", lr)
	}
	if !rr.Enabled || rr.Addr != ":9000" || rr.WebRoot != "" || rr.TLSCert != "" {
		t.Fatalf("a remote peer must not be handed server paths, got %+v", rr)
	}
}

// The policy withholds shell tools only where there is no real sandbox.
func TestApplyRuntime_DisableBashOnlyWhenUnsandboxed(t *testing.T) {
	deps := newTestDeps(t)
	deps.settings.OnRuntimeChange(deps.projects.ApplyRuntime)

	unsandboxed, err := deps.projects.Get(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sandboxed, err := deps.projects.Get(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unsandboxed.SandboxTier = string(sandbox.ModeLocalRestr)
	sandboxed.SandboxTier = string(sandbox.ModeBubblewrap)
	ipc := protocol.NewIPCStream(nil, &bytes.Buffer{})
	shell := func(pc *ProjectContext, session string) bool { return pc.Registry.Get(ipc, session).ShellDisabled() }

	if shell(unsandboxed, "a") || shell(sandboxed, "b") {
		t.Fatal("bash must stay allowed by default")
	}
	if _, err := deps.settings.UpdateRuntime(patch(t, `{"disableBashWhenUnsandboxed":true}`)); err != nil {
		t.Fatal(err)
	}
	if !shell(unsandboxed, "a") {
		t.Fatal("an already-open session in an unsandboxed project kept its shell tools")
	}
	if shell(sandboxed, "b") {
		t.Fatal("a sandboxed project lost its shell tools")
	}
	if !shell(unsandboxed, "new") {
		t.Fatal("a new session in an unsandboxed project should start with shell disabled")
	}
	if _, err := deps.settings.UpdateRuntime(patch(t, `{"disableBashWhenUnsandboxed":false}`)); err != nil {
		t.Fatal(err)
	}
	if shell(unsandboxed, "a") {
		t.Fatal("turning the setting off did not restore bash")
	}
}

func TestSetRemoteAccess_PersistsRestartsAndRollsBack(t *testing.T) {
	deps := newTestDeps(t)
	deps.remote.stateDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(deps.remote.Stop)

	addr := "127.0.0.1:0"
	got, err := applyRemoteAccess(ctx, deps, protocol.SetRemoteAccessData{Enabled: true, Addr: &addr})
	if err != nil || !got.Enabled || got.Addr != addr {
		t.Fatalf("enable = %+v, %v", got, err)
	}
	first := deps.remote.ln
	if first == nil {
		t.Fatal("listener not running")
	}
	if s := deps.settings.Remote(); !s.Enabled || s.Addr != addr {
		t.Fatalf("not persisted: %+v", s)
	}

	other := "localhost:0"
	if _, err := applyRemoteAccess(ctx, deps, protocol.SetRemoteAccessData{Enabled: true, Addr: &other}); err != nil {
		t.Fatal(err)
	}
	if deps.remote.ln == nil || deps.remote.ln == first {
		t.Fatal("changing the address of a running listener must restart it")
	}

	// An address that cannot be bound leaves the stored settings as they were.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyAddr := busy.Addr().String()
	if _, err := applyRemoteAccess(ctx, deps, protocol.SetRemoteAccessData{Enabled: true, Addr: &busyAddr}); err == nil {
		t.Fatal("expected the bind to fail")
	}
	if s := deps.settings.Remote(); s.Addr != other || !s.Enabled {
		t.Fatalf("a failed change was persisted: %+v", s)
	}
	if deps.remote.ln == nil {
		t.Fatal("the previous listener should be back after a failed change")
	}

	if got, err := applyRemoteAccess(ctx, deps, protocol.SetRemoteAccessData{Enabled: false}); err != nil || got.Enabled {
		t.Fatalf("disable = %+v, %v", got, err)
	}
	if deps.remote.ln != nil || deps.settings.Remote().Enabled {
		t.Fatal("disable did not stop and persist")
	}
	// TLS files come as a pair.
	cert := "/tmp/only-a-cert.pem"
	if _, err := applyRemoteAccess(ctx, deps, protocol.SetRemoteAccessData{Enabled: false, TLSCert: &cert}); err == nil {
		t.Fatal("a certificate without a key must be rejected")
	}
}

func TestSeedFromFlags_SeedsOnceAndNeverOverrides(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := provider.NewProviderStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	flags := launchFlags{
		provider: "anthropic", model: "claude-x", apiKey: "sk-test", maxSessionCostUSD: 3,
		tcpAddr: ":7999", tlsCert: "c.pem", tlsKey: "k.pem", webRoot: "/w",
	}
	set := map[string]bool{"provider": true, "model": true, "api-key": true, "max-session-cost-usd": true, "listen": true,
		"tls-cert": true, "tls-key": true, "web-root": true}
	seedFromFlags(flags, set, s, store)

	if s.Runtime().MaxSessionCostUSD != 3 {
		t.Fatalf("cost = %v", s.Runtime().MaxSessionCostUSD)
	}
	if d := s.DefaultProvider(); d.Name != "default" || d.Model != "claude-x" {
		t.Fatalf("default provider = %+v", d)
	}
	if cfg, ok := store.Get("default"); !ok || cfg.HTTPProvider != "anthropic" || cfg.APIKey != "sk-test" {
		t.Fatalf("registered provider = %+v %v", cfg, ok)
	}
	if r := s.Remote(); !r.Enabled || r.Addr != ":7999" || r.TLSCert != "c.pem" || r.WebRoot != "/w" {
		t.Fatalf("remote = %+v", r)
	}

	// A client changes things; the next launch with different flags changes nothing.
	if _, err := s.UpdateRuntime(patch(t, `{"maxSessionCostUsd":0}`)); err != nil {
		t.Fatal(err)
	}
	_ = s.SetDefaultProvider("", "")
	_, _ = s.UpdateRemote(func(r *RemoteSettings) { r.Enabled = false })
	flags2 := launchFlags{provider: "openai", model: "other", apiKey: "sk-other", maxSessionCostUSD: 99, tcpAddr: ":1"}
	seedFromFlags(flags2, map[string]bool{"provider": true, "model": true, "api-key": true, "max-session-cost-usd": true, "listen": true}, s, store)
	if s.Runtime().MaxSessionCostUSD != 0 {
		t.Fatal("a flag overrode a stored runtime setting")
	}
	if d := s.DefaultProvider(); d.Name != "" {
		t.Fatalf("a flag overrode a deliberately cleared default provider: %+v", d)
	}
	if r := s.Remote(); r.Enabled || r.Addr != ":7999" {
		t.Fatalf("a flag overrode stored remote settings: %+v", r)
	}
}

func TestSetDefaultProvider_ReachesNewAndProviderlessSessions(t *testing.T) {
	deps := newTestDeps(t)
	deps.projects.SetDefaultSource(defaultProviderSource(deps.settings, deps.providerStore, nil))
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	ipc := protocol.NewIPCStream(nil, &bytes.Buffer{})
	before := pc.Registry.Get(ipc, "early")
	if before.ProviderName() != "" {
		t.Fatalf("a bare server should have no provider, has %q", before.ProviderName())
	}

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventSetDefaultProvider, protocol.SetDefaultProviderData{ProviderName: "ghost"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("an unregistered provider must be rejected, got %+v", envs)
	}

	if err := deps.providerStore.Register(provider.ProviderConfig{Name: "mine", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", APIKey: "k", BaseURL: "http://localhost:1/v1"}); err != nil {
		t.Fatal(err)
	}
	envs = runDispatch(t, deps, envelopeJSON(t, protocol.EventSetDefaultProvider, protocol.SetDefaultProviderData{ProviderName: "mine", Model: "m"}))
	if len(envs) != 1 || envs[0].Type != protocol.EventDefaultProviderSet {
		t.Fatalf("reply = %+v", envs)
	}
	if d := deps.settings.DefaultProvider(); d.Name != "mine" || d.Model != "m" {
		t.Fatalf("stored = %+v", d)
	}
	if before.ProviderName() != "mine" {
		t.Fatalf("a live session with no provider did not pick up the default, has %q", before.ProviderName())
	}
	if got := pc.Registry.Get(ipc, "fresh").ProviderName(); got != "mine" {
		t.Fatalf("a new session got provider %q, want mine", got)
	}

	runDispatch(t, deps, envelopeJSON(t, protocol.EventSetDefaultProvider, protocol.SetDefaultProviderData{}))
	if d := deps.settings.DefaultProvider(); d.Name != "" {
		t.Fatalf("clearing left %+v", d)
	}
}

// The web root a browser is served comes from the stored remote settings, so
// changing it needs no restart; the connDeps field only backs tests.
func TestWebRootDir_StoredSettingWins(t *testing.T) {
	deps := newTestDeps(t)
	deps.webRoot = "/from/field"
	if got := deps.webRootDir(); got != "/from/field" {
		t.Fatalf("with nothing stored, webRootDir = %q", got)
	}
	if _, err := deps.settings.UpdateRemote(func(r *RemoteSettings) { r.WebRoot = "/from/settings" }); err != nil {
		t.Fatal(err)
	}
	if got := deps.webRootDir(); got != "/from/settings" {
		t.Fatalf("webRootDir = %q, want the stored one", got)
	}
}

func TestProcessLimitsReachTheirPackages(t *testing.T) {
	s, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pricing := provider.NewPricingCache("")
	s.OnRuntimeChange(applyProcessLimits(pricing))
	t.Cleanup(func() {
		sandbox.SetLimits(sandbox.Limits{})
		provider.SetPoolLimits(0, 0)
	})
	if _, err := s.UpdateRuntime(patch(t, `{"execTimeoutSec":5,"execOutputMaxBytes":8192,"terminalMemoryMaxMB":128,"terminalPidsMax":32,"terminalReadMaxChars":900,"terminalHistoryKB":64,"poolIdleMinutes":7,"poolMaxLive":3}`)); err != nil {
		t.Fatal(err)
	}
	l := sandbox.CurrentLimits()
	if l.ExecTimeout != 5*time.Second || l.ExecOutputMaxBytes != 8192 || l.TerminalMemoryBytes != 128<<20 ||
		l.TerminalPids != 32 || l.TerminalReadMaxChars != 900 || l.TerminalHistoryBytes != 64<<10 {
		t.Fatalf("sandbox limits = %+v", l)
	}
	if st := provider.NativePoolStats(); st.Max != 3 || st.IdleTimeoutSec != 7*60 {
		t.Fatalf("pool limits = %+v", st)
	}
}
