package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"yoakeland/internal/agent"
	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// sendRuntimeState tells a freshly connected peer the server settings that
// live outside the older per-setting events: runtime_settings,
// default_provider_set and remote_access_set. network is the peer's ("unix"
// peers also get the TLS and web-root paths).
func sendRuntimeState(ipc *protocol.IPCStream, deps connDeps, network string) {
	_ = ipc.SendEvent(protocol.EventRuntimeSettings, "", "", "", deps.settings.Runtime())
	_ = ipc.SendEvent(protocol.EventDefaultProviderSet, "", "", "", defaultProviderData(deps.settings))
	_ = ipc.SendEvent(protocol.EventRemoteAccessSet, "", "", "", remoteAccessData(deps.settings.Remote(), network == "unix"))
}

func defaultProviderData(s *ServerSettings) protocol.DefaultProviderSetData {
	d := s.DefaultProvider()
	return protocol.DefaultProviderSetData{ProviderName: d.Name, Model: d.Model}
}

func remoteAccessData(cfg RemoteSettings, local bool) protocol.RemoteAccessSetData {
	out := protocol.RemoteAccessSetData{Enabled: cfg.Enabled, Addr: cfg.Addr}
	if local {
		out.TLSCert, out.TLSKey, out.WebRoot = cfg.TLSCert, cfg.TLSKey, cfg.WebRoot
	}
	return out
}

// broadcastRemoteAccess pushes the remote-access state to every peer but
// except (who got a direct reply).
func broadcastRemoteAccess(deps connDeps, except string) {
	cfg := deps.settings.Remote()
	for _, p := range deps.peers.All() {
		if p.ID != except {
			_ = p.ipc.SendEvent(protocol.EventRemoteAccessSet, "", "", "", remoteAccessData(cfg, p.Network == "unix"))
		}
	}
}

// handleSetRuntimeSettings applies a partial RuntimeSettings update. Any
// authenticated peer may change these (the server is one person's), unlike
// remote access and credentials, which stay local-socket-only. The reply and
// the broadcast carry the stored (clamped) values.
func handleSetRuntimeSettings(ipc *protocol.IPCStream, env *protocol.Envelope, peerID string, deps connDeps) {
	var patch protocol.SetRuntimeSettingsData
	if err := json.Unmarshal(env.Data, &patch); err != nil || patch == nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "malformed set_runtime_settings payload (expected an object of settings)",
		})
		return
	}
	rs, err := deps.settings.UpdateRuntime(patch)
	if err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: fmt.Sprintf("set_runtime_settings: %v", err),
		})
		return
	}
	// Which settings, never to what: some of them are paths and keys.
	deps.record(ipc, peerID, audit.Event{Kind: audit.SettingsChange, Target: "runtime", Outcome: "ok", Detail: map[string]any{"keys": changedKeys(env.Data)}})
	_ = ipc.SendEvent(protocol.EventRuntimeSettings, env.SessionID, env.TurnID, "", rs)
	deps.peers.BroadcastEvent(peerID, protocol.EventRuntimeSettings, "", rs)
}

// handleSetDefaultProvider picks (or, with an empty name, clears) the
// registered provider new sessions start on.
func handleSetDefaultProvider(ipc *protocol.IPCStream, env *protocol.Envelope, peerID string, deps connDeps) {
	var data protocol.SetDefaultProviderData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "malformed set_default_provider payload",
		})
		return
	}
	if data.ProviderName != "" {
		if _, ok := deps.providerStore.Get(data.ProviderName); !ok {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown provider %q", data.ProviderName),
			})
			return
		}
	} else {
		data.Model = ""
	}
	if err := deps.settings.SetDefaultProvider(data.ProviderName, data.Model); err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: fmt.Sprintf("failed to save default provider: %v", err),
		})
		return
	}
	deps.projects.ApplyDefaultProvider()
	deps.record(ipc, peerID, audit.Event{Kind: audit.SettingsChange, Target: "defaultProvider", Outcome: "ok", Detail: map[string]any{"keys": []string{"defaultProvider"}, "provider": data.ProviderName, "model": data.Model}})
	out := defaultProviderData(deps.settings)
	_ = ipc.SendEvent(protocol.EventDefaultProviderSet, env.SessionID, env.TurnID, "", out)
	deps.peers.BroadcastEvent(peerID, protocol.EventDefaultProviderSet, "", out)
}

// applyRemoteAccess stores a set_remote_access request and brings the
// listener in line with it: started, stopped, or restarted when its address
// or certificate changed. A listener that cannot start rolls the stored
// settings back, so "enabled" never persists for something that is not running.
func applyRemoteAccess(ctx context.Context, deps connDeps, data protocol.SetRemoteAccessData) (RemoteSettings, error) {
	prev := deps.settings.Remote()
	cfg, err := deps.settings.UpdateRemote(func(r *RemoteSettings) {
		r.Enabled = data.Enabled
		if data.Addr != nil {
			r.Addr = *data.Addr
		}
		if data.TLSCert != nil {
			r.TLSCert = *data.TLSCert
		}
		if data.TLSKey != nil {
			r.TLSKey = *data.TLSKey
		}
		if data.WebRoot != nil {
			r.WebRoot = *data.WebRoot
		}
	})
	if err != nil {
		return prev, err
	}
	deps.remote.apply(cfg)
	if !cfg.Enabled {
		deps.remote.Stop()
		return cfg, nil
	}
	if cfg.Addr != prev.Addr || cfg.TLSCert != prev.TLSCert || cfg.TLSKey != prev.TLSKey {
		deps.remote.Stop() // a running listener keeps its old address and certificate
	}
	if err := deps.remote.Start(ctx); err != nil {
		_, _ = deps.settings.UpdateRemote(func(r *RemoteSettings) { *r = prev })
		deps.remote.apply(prev)
		if prev.Enabled {
			_ = deps.remote.Start(ctx)
		}
		return prev, err
	}
	return cfg, nil
}

// webRootDir is the directory served to browsers on the remote listener, read
// per connection so changing it needs no restart.
func (d connDeps) webRootDir() string {
	if d.settings != nil {
		if w := d.settings.Remote().WebRoot; w != "" {
			return w
		}
	}
	return d.webRoot
}

// defaultProviderSource resolves the persisted default_provider setting into
// a live provider for a session in project root, or nil when none is set (or
// it no longer builds), which sends the registry back to the provider the
// environment auto-detected at startup.
func defaultProviderSource(s *ServerSettings, store *provider.ProviderStore, hub *statusHub) func(root string) (agent.ModelProvider, string) {
	return func(root string) (agent.ModelProvider, string) {
		d := s.DefaultProvider()
		if d.Name == "" {
			return nil, ""
		}
		cfg, ok := store.Get(d.Name)
		if !ok {
			return nil, ""
		}
		built, err := provider.BuildProvider(cfg, d.Model, root)
		if err != nil {
			logWarn("default provider %q does not build: %v", d.Name, err)
			return nil, ""
		}
		if cli, ok := built.(*provider.CLISubprocessProvider); ok && hub != nil {
			name, kind := d.Name, string(cfg.Kind)
			cli.OnRateLimits = func(rl protocol.ProviderRateLimits) {
				rl.Kind = kind
				hub.SetRateLimits(name, rl)
			}
		}
		return built, d.Name
	}
}

// launchFlags are the deprecated launch flags that used to configure the
// server. They are runtime settings now (set_runtime_settings,
// set_default_provider, set_remote_access, set_remote_password); a flag only
// hands its value over once, when nothing has been stored yet, and never
// overrides what a client has since set.
type launchFlags struct {
	provider, model, apiKey, baseURL string
	maxSessionCostUSD                float64
	tcpAddr, tlsCert, tlsKey         string
	webRoot                          string
}

// seedFromFlags migrates launch flags into the persisted settings and logs
// a deprecation notice for each one given. set names the flags (and env vars)
// that were explicitly provided.
func seedFromFlags(f launchFlags, set map[string]bool, s *ServerSettings, store *provider.ProviderStore) {
	note := func(flagName, replacement string, seeded bool) {
		result := "stored it as the setting"
		if !seeded {
			result = "left it alone because the server already has a stored value"
		}
		logWarnDetail(
			fmt.Sprintf("%s is deprecated: use %s", flagName, replacement),
			fmt.Sprintf("%s is deprecated. On first launch it %s; it never overrides a value a client has set since. Change it at runtime with %s.", flagName, result, replacement),
		)
	}

	if set["max-session-cost-usd"] {
		seeded := s.SeedRuntime(func(rs *protocol.RuntimeSettings) { rs.MaxSessionCostUSD = f.maxSessionCostUSD })
		note("-max-session-cost-usd", "set_runtime_settings {maxSessionCostUsd}", seeded)
	}

	if set["provider"] || set["model"] || set["api-key"] || set["base-url"] {
		seeded := false
		if s.DefaultProviderUnset() {
			if http := providerFlagBackend(f.provider, f.baseURL); http != "" {
				err := store.Register(provider.ProviderConfig{
					Name: "default", Kind: provider.ProviderKindAPIKey, HTTPProvider: http,
					BaseURL: f.baseURL, APIKey: f.apiKey, AuthMethod: "manual",
				})
				if err != nil {
					logWarn("could not register the provider from the launch flags: %v", err)
				} else {
					seeded = s.SeedDefaultProvider("default", f.model)
				}
			}
		}
		for _, name := range []string{"provider", "model", "api-key", "base-url"} {
			if set[name] {
				note("-"+name, "register_provider + set_default_provider", seeded)
			}
		}
	}

	if f.tcpAddr != "" || set["tls-cert"] || set["tls-key"] || set["web-root"] {
		seeded := s.SeedRemote(RemoteSettings{
			Enabled: f.tcpAddr != "", Addr: f.tcpAddr, TLSCert: f.tlsCert, TLSKey: f.tlsKey, WebRoot: f.webRoot,
		})
		for _, name := range []string{"tls-cert", "tls-key", "web-root"} {
			if set[name] {
				note("-"+name, "set_remote_access {"+map[string]string{"tls-cert": "tlsCert", "tls-key": "tlsKey", "web-root": "webRoot"}[name]+"}", seeded)
			}
		}
		if f.tcpAddr != "" {
			note("-listen "+f.tcpAddr, "set_remote_access {enabled, addr}", seeded)
		}
	}
}

// providerFlagBackend names the HTTP backend a -provider/-base-url pair
// selects, the way provider.ResolveProvider does, or "" if there is none.
func providerFlagBackend(name, baseURL string) string {
	if name != "" && name != "auto" {
		return name
	}
	if baseURL != "" || os.Getenv("OPENAI_BASE_URL") != "" {
		return "openai"
	}
	for _, c := range []struct{ env, name string }{
		{"ANTHROPIC_API_KEY", "anthropic"}, {"OPENROUTER_API_KEY", "openrouter"}, {"OPENAI_API_KEY", "openai"},
	} {
		if os.Getenv(c.env) != "" {
			return c.name
		}
	}
	return ""
}
