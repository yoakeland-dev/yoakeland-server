package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
	"yoakeland/internal/fsutil"
	"yoakeland/internal/protocol"
)

// TokenRecord is one named person's TCP auth credential, plus the identity
// cached against it — see register_token/list_tokens/remove_token. Name is
// fixed at registration (rename = revoke + re-register, deliberately simple);
// Avatar is mutable via set_peer_avatar so it survives reconnects instead of
// resetting every time.
type TokenRecord struct {
	Token  string `json:"token"`
	Email  string `json:"email,omitempty"`
	Avatar string `json:"avatar,omitempty"`

	// Pairing tokens are made for one phone about to scan a QR code: unclaimed they stop working
	// at ExpiresAt, and the first device to use one is the only device that ever can (DeviceID).
	Pairing   bool   `json:"pairing,omitempty"`
	ExpiresAt int64  `json:"expiresAt,omitempty"` // unix seconds; 0 once claimed
	DeviceID  string `json:"deviceId,omitempty"`

	// Scope overrides the default tier: "full" or "limited". Empty means a pairing credential is
	// limited (see remoteAllowed) and any other is full. Set with set_peer_scope or approve_pairing.
	Scope string `json:"scope,omitempty"`
}

// Restricted reports whether devices on this credential are held to remoteAllowed.
func (r TokenRecord) Restricted() bool {
	if r.Scope != "" {
		return r.Scope == "limited"
	}
	return r.Pairing
}

// PairingTTL is how long a pairing QR code works before anyone uses it.
const PairingTTL = 10 * time.Minute

// ServerSettings persists the small set of runtime knobs a client can change
// via set_server_identity/set_remote_password/register_token as
// <stateDir>/server.json, so they survive a restart instead of resetting to
// flag defaults every time — same directory and 0600-on-write pattern as
// provider.ProviderStore, since a token is a credential the same way an API
// key is.
type ServerSettings struct {
	mu       sync.Mutex
	filePath string

	Identity string                 `json:"identity,omitempty"`
	Tokens   map[string]TokenRecord `json:"tokens,omitempty"`

	// ServerID is a stable identifier minted once, the first time this
	// -state-dir is ever used, and never changed or exposed via any setter
	// — unlike Identity (a mutable display name), its only value is that it
	// can't change, so a client reaching this server under two different
	// addresses (a LAN IP one time, a VPN hostname another) can still tell
	// it's the same daemon, and so a client holding several simultaneous
	// connections has a stable key to store per-server state under instead
	// of an address string.
	ServerID string `json:"serverId,omitempty"`

	// AutoGenerateTitle is the server-wide default for a first user_prompt
	// that omits its own autoGenerateTitle (SPEC.md §3.1) — nil (the
	// unset-file case) means "on", matching that same per-request default,
	// so a fresh server behaves identically whether or not this was ever
	// touched.
	AutoGenerateTitle *bool `json:"autoGenerateTitle,omitempty"`

	// ModelRoles is the server-wide model role table (set_model_roles,
	// SPEC.md §4f), keyed by role name.
	ModelRoles map[string]protocol.ModelRole `json:"modelRoles,omitempty"`

	// Notify is the notification policy (set_notify_prefs, notify_prefs);
	// nil means defaultNotifyPrefs. Accessors live in notify_prefs.go.
	Notify *protocol.NotifyPrefsData `json:"notify,omitempty"`

	// The server's runtime tunables, remote-listener configuration and default
	// provider (runtime_settings.go). Each is nil until first stored, which is
	// what lets a deprecated launch flag seed it exactly once.
	RuntimeState         *runtimeState           `json:"runtime,omitempty"`
	RemoteState          *RemoteSettings         `json:"remote,omitempty"`
	DefaultProviderState *DefaultProviderSetting `json:"defaultProvider,omitempty"`
	runtimeHooks         []runtimeHook

	// Token is the legacy single-secret field from before per-person
	// tokens existed. Only ever read at load time, to migrate into
	// Tokens["default"] — GetToken/SetToken/save all operate on Tokens
	// from then on, so a single-user setup (-token, set_remote_password)
	// keeps working with zero config changes.
	Token string `json:"token,omitempty"`
}

const defaultTokenName = "default"

// NewServerSettings opens (or creates) the settings file in dir. Identity
// defaults to the machine's hostname when nothing has been persisted or
// passed via -state-dir yet, so a freshly started server always has a
// non-empty display name to show peers.
func NewServerSettings(dir string) (*ServerSettings, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &ServerSettings{filePath: filepath.Join(dir, "server.json"), Tokens: map[string]TokenRecord{}}
	if data, err := os.ReadFile(s.filePath); err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			// PRV-12/NET-16: set the unreadable file aside rather than let the
			// next save overwrite it, so its tokens stay recoverable by hand.
			aside := fmt.Sprintf("%s.corrupt-%d", s.filePath, time.Now().Unix())
			if rerr := os.Rename(s.filePath, aside); rerr != nil {
				return nil, fmt.Errorf("server.json failed to parse and could not be set aside: %w", rerr)
			}
			logWarn("server settings: server.json failed to parse, moved to %s; starting empty: %v", aside, err)
			s.Identity, s.Token, s.Tokens = "", "", map[string]TokenRecord{}
			s.RuntimeState, s.RemoteState, s.DefaultProviderState = nil, nil, nil
		}
	}
	if s.RuntimeState != nil {
		s.RuntimeState.RuntimeSettings = normalizeRuntimeSettings(s.RuntimeState.RuntimeSettings)
	}
	if s.Tokens == nil {
		s.Tokens = map[string]TokenRecord{}
	}
	if s.Token != "" {
		if _, ok := s.Tokens[defaultTokenName]; !ok {
			s.Tokens[defaultTokenName] = TokenRecord{Token: s.Token}
		}
		s.Token = ""
	}
	if s.Identity == "" {
		if host, err := os.Hostname(); err == nil {
			s.Identity = host
		}
	}
	if s.ServerID == "" {
		id, err := generateAuthToken() // same crypto/rand+hex idiom, same package
		if err != nil {
			return nil, fmt.Errorf("generating server id: %w", err)
		}
		s.ServerID = id
		// Unlike Identity, which is allowed to re-derive from hostname every
		// launch until explicitly set, this must persist immediately — the
		// whole point of ServerID is that it's stable from the moment it's
		// first minted, including on an existing install's first launch
		// after upgrading to a version that has this field at all.
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *ServerSettings) GetIdentity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Identity
}

func (s *ServerSettings) SetIdentity(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Identity = name
	return s.save()
}

// GetServerID returns this server's stable identifier. Deliberately no
// setter: a client relies on this value never changing.
func (s *ServerSettings) GetServerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ServerID
}

// GetAutoGenerateTitle returns the server-wide default, true unless an
// operator has explicitly turned it off via SetAutoGenerateTitle.
func (s *ServerSettings) GetAutoGenerateTitle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AutoGenerateTitle == nil || *s.AutoGenerateTitle
}

func (s *ServerSettings) SetAutoGenerateTitle(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AutoGenerateTitle = &enabled
	return s.save()
}

func (s *ServerSettings) GetModelRoles() map[string]protocol.ModelRole {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.ModelRoles)
}

func (s *ServerSettings) SetModelRoles(roles map[string]protocol.ModelRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ModelRoles = maps.Clone(roles)
	return s.save()
}

// GetToken returns the "default" token — the one -token/set_remote_password
// manage, kept for every existing single-shared-token caller.
func (s *ServerSettings) GetToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Tokens[defaultTokenName].Token
}

// SetToken overwrites the "default" token, preserving any email/avatar
// already cached against it.
func (s *ServerSettings) SetToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.Tokens[defaultTokenName]
	rec.Token = token
	s.Tokens[defaultTokenName] = rec
	return s.save()
}

// LookupToken resolves a presented token to the name it was registered
// under, using constantTimeTokenEqual for every comparison so a mismatch
// doesn't leak timing information about which entry (if any) it's closest
// to. Safe to call with network == "unix" callers passing an empty token —
// it simply won't match anything.
func (s *ServerSettings) LookupToken(token string) (name string, rec TokenRecord, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, r := range s.Tokens {
		if constantTimeTokenEqual(token, r.Token) {
			return n, r, true
		}
	}
	return "", TokenRecord{}, false
}

// AddToken registers a new named credential. name must be non-empty and not
// already registered — rename is deliberately unsupported (revoke + re-add
// instead), so there's no ambiguity about what "the same person" means.
func (s *ServerSettings) AddToken(name, token, email string) error {
	if name == "" {
		return fmt.Errorf("token name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.Tokens[name]; exists {
		return fmt.Errorf("a token named %q already exists", name)
	}
	s.Tokens[name] = TokenRecord{Token: token, Email: email}
	return s.save()
}

// AddPairingToken is AddToken for a one-device pairing token (see TokenRecord).
func (s *ServerSettings) AddPairingToken(name, token, email string) error {
	if name == "" {
		return fmt.Errorf("token name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked()
	if _, exists := s.Tokens[name]; exists {
		return fmt.Errorf("a token named %q already exists", name)
	}
	s.Tokens[name] = TokenRecord{Token: token, Email: email, Pairing: true, ExpiresAt: time.Now().Add(PairingTTL).Unix()}
	return s.save()
}

// ClaimPairing binds a pairing token to the first device that uses it. It fails for a token that
// expired unclaimed (and removes it) or one another device already holds; the same device
// presenting it again is fine.
func (s *ServerSettings) ClaimPairing(name, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	if !ok || !rec.Pairing {
		return fmt.Errorf("this pairing code is no longer valid")
	}
	if rec.DeviceID != "" {
		if rec.DeviceID == deviceID {
			return nil
		}
		return fmt.Errorf("this pairing code was already used")
	}
	if time.Now().Unix() > rec.ExpiresAt {
		delete(s.Tokens, name)
		_ = s.save()
		return fmt.Errorf("this pairing code expired")
	}
	if deviceID == "" {
		return fmt.Errorf("this app is too old to pair; update it")
	}
	rec.DeviceID, rec.ExpiresAt = deviceID, 0
	s.Tokens[name] = rec
	return s.save()
}

// purgeExpiredLocked drops pairing tokens nobody used in time, so a QR code that was never
// scanned doesn't linger as a credential.
func (s *ServerSettings) purgeExpiredLocked() {
	now := time.Now().Unix()
	for n, r := range s.Tokens {
		if r.Pairing && r.DeviceID == "" && now > r.ExpiresAt {
			delete(s.Tokens, n)
		}
	}
}

// ListTokenNames returns every registered token's name/email/whether it has
// a cached avatar — never the raw token, same scrubbing precedent as
// provider_list's apiKey.
func (s *ServerSettings) ListTokenNames() []TokenSummaryInternal {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked()
	out := make([]TokenSummaryInternal, 0, len(s.Tokens))
	for n, r := range s.Tokens {
		out = append(out, TokenSummaryInternal{Name: n, Email: r.Email, HasAvatar: r.Avatar != ""})
	}
	return out
}

// TokenSummaryInternal mirrors protocol.TokenSummary — kept as a plain
// package-local type so settings.go doesn't need to import internal/protocol
// just for this shape; main.go converts it at the dispatch boundary.
type TokenSummaryInternal struct {
	Name      string
	Email     string
	HasAvatar bool
}

// RemoveToken deletes a named credential, idempotently (removing a name
// that's already gone is a success, not an error — same precedent as
// remove_provider: revoking access that's already revoked is a benign
// no-op).
func (s *ServerSettings) RemoveToken(name string) (existed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, existed = s.Tokens[name]; !existed {
		return false, nil
	}
	delete(s.Tokens, name)
	return true, s.save()
}

// RemoveUnclaimedPairing deletes a pairing credential only while no device holds it. A device that
// waited for approval must not take down a code another device has since claimed.
func (s *ServerSettings) RemoveUnclaimedPairing(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.Tokens[name]; ok && rec.Pairing && rec.DeviceID == "" {
		delete(s.Tokens, name)
		_ = s.save()
	}
}

// SetTokenAvatar persists an avatar against a named token so it survives
// reconnects — the "cache avatars" half of set_peer_avatar for a tokened
// (TCP, per-person) peer. No-op success if the name is unknown, mirroring
// PeerRegistry.SetAvatar's "report whether it existed" shape rather than
// erroring on a race with a since-removed token.
func (s *ServerSettings) SetTokenAvatar(name, avatar string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	if !ok {
		return false
	}
	rec.Avatar = avatar
	s.Tokens[name] = rec
	_ = s.save()
	return true
}

// SetTokenScope makes a credential full (unrestricted) or limited. False if no such credential.
func (s *ServerSettings) SetTokenScope(name string, full bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	if !ok {
		return false
	}
	rec.Scope = "limited"
	if full {
		rec.Scope = "full"
	}
	s.Tokens[name] = rec
	_ = s.save()
	return true
}

func (s *ServerSettings) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.filePath, data, 0600)
}
