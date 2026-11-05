package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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

	// UserID is the server-owned person this credential logs in as (see User). MaxTier, when set,
	// caps the device below its person's tier; Deny lists capabilities taken away on this device.
	UserID   string   `json:"userId,omitempty"`
	MaxTier  string   `json:"maxTier,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	LastSeen int64    `json:"lastSeen,omitempty"` // unix seconds, set on auth and on disconnect
	PairedBy string   `json:"pairedBy,omitempty"` // user id of whoever created or approved it

	// Scope is the pre-users tier ("full" or "limited"). It is only read on load, to migrate the
	// credential into a User, and never written back.
	Scope string `json:"scope,omitempty"`
}

// legacyRestricted is what Scope and Pairing meant before users existed.
func (r TokenRecord) legacyRestricted() bool {
	if r.Scope != "" {
		return r.Scope == "limited"
	}
	return r.Pairing
}

// Tiers, lowest first.
const (
	TierLimited = "limited"
	TierTrusted = "trusted"
	TierFull    = "full"

	ownerID = "owner"
)

// User is a person the server knows. A credential (device) logs in as one.
type User struct {
	ID        string         `json:"id"` // "owner" or "u-<random>"
	Name      string         `json:"name"`
	Avatar    string         `json:"avatar,omitempty"` // emoji or data URL, capped at maxAvatarLen
	Email     string         `json:"email,omitempty"`
	Tier      string         `json:"tier"`
	Caps      map[string]any `json:"caps,omitempty"`      // only what differs from the tier default
	AliasOf   string         `json:"aliasOf,omitempty"`   // set by merge_users; resolve before any use
	Names     []string       `json:"names,omitempty"`     // earlier names, for matching old messages
	CreatedBy string         `json:"createdBy,omitempty"` // user id
	CreatedAt int64          `json:"createdAt"`
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
	Icon     *protocol.ServerIcon   `json:"icon,omitempty"`
	Tokens   map[string]TokenRecord `json:"tokens,omitempty"`

	// Users are the people credentials log in as; LimitedDefaults overlays the Limited tier's
	// default capabilities for everyone on it.
	Users           map[string]User `json:"users,omitempty"`
	LimitedDefaults map[string]any  `json:"limitedDefaults,omitempty"`
	// ProviderOwners remembers who added each provider a non-Full person registered, so they
	// may change only their own.
	ProviderOwners map[string]string `json:"providerOwners,omitempty"`

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

	// LimitAction is what happens when a turn stops at a usage limit (limit.go): "ask" (the
	// default), "continue" or "stop".
	LimitAction string `json:"limitAction,omitempty"`

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
			s.Users, s.LimitedDefaults = nil, nil
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
	if s.migrate() {
		if err := s.save(); err != nil {
			return nil, err
		}
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

func (s *ServerSettings) GetIcon() *protocol.ServerIcon {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Icon
}

// iconName is the shape of ServerIcon.Name: it's shown nowhere as text, only looked up in the
// client's icon set, so anything else is a malformed request rather than something to repair.
var iconName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// SetIcon stores the icon. Hue must be 0-359; an empty icon clears it.
func (s *ServerSettings) SetIcon(ic protocol.ServerIcon) error {
	if ic.Name != "" && !iconName.MatchString(ic.Name) {
		return fmt.Errorf("invalid icon name %q", ic.Name)
	}
	if ic.Hue != nil && (*ic.Hue < 0 || *ic.Hue > 359) {
		return fmt.Errorf("icon hue %d out of range 0-359", *ic.Hue)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ic.Name == "" && ic.Hue == nil {
		s.Icon = nil
	} else {
		s.Icon = &ic
	}
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
	return s.AddTokenFor(name, token, email, "", "")
}

// AddTokenFor is AddToken for a credential that logs in as userID (made by pairedBy). With no
// userID it makes a new full-tier user named after the token, as an unscoped credential always was.
func (s *ServerSettings) AddTokenFor(name, token, email, userID, pairedBy string) error {
	if name == "" {
		return fmt.Errorf("token name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.Tokens[name]; exists {
		return fmt.Errorf("a token named %q already exists", name)
	}
	if userID == "" {
		u := s.newUserLocked(name, email, "", TierFull, pairedBy)
		userID = u.ID
	}
	s.Tokens[name] = TokenRecord{Token: token, Email: email, UserID: userID, PairedBy: pairedBy}
	return s.save()
}

// AddPairingToken is AddToken for a one-device pairing token (see TokenRecord).
func (s *ServerSettings) AddPairingToken(name, token, email string) error {
	return s.AddPairingTokenFor(name, token, email, "", "", "")
}

// AddPairingTokenFor is AddPairingToken for a credential that logs in as userID, with maxTier as
// the device's ceiling (empty for none). With no userID it makes a new limited user named after
// the token, as a pairing credential always was.
func (s *ServerSettings) AddPairingTokenFor(name, token, email, userID, maxTier, pairedBy string) error {
	if name == "" {
		return fmt.Errorf("token name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked()
	if _, exists := s.Tokens[name]; exists {
		return fmt.Errorf("a token named %q already exists", name)
	}
	if userID == "" {
		u := s.newUserLocked(name, email, "", TierLimited, pairedBy)
		userID = u.ID
	}
	s.Tokens[name] = TokenRecord{Token: token, Email: email, Pairing: true, ExpiresAt: time.Now().Add(PairingTTL).Unix(),
		UserID: userID, MaxTier: maxTier, PairedBy: pairedBy}
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
// scanned doesn't linger as a credential. The person made for it goes too, if nothing else
// uses them. Reports whether anything was dropped.
func (s *ServerSettings) purgeExpiredLocked() bool {
	now := time.Now().Unix()
	dropped := false
	for n, r := range s.Tokens {
		if r.Pairing && r.DeviceID == "" && now > r.ExpiresAt {
			delete(s.Tokens, n)
			dropped = true
			// ponytail: a person made for a code is recognised by its creation time matching the
			// code's; an extra field would say it outright.
			if u, ok := s.Users[r.UserID]; ok && u.ID != ownerID && u.CreatedAt >= r.ExpiresAt-int64(PairingTTL/time.Second)-2 && u.CreatedAt <= r.ExpiresAt && !s.userHasTokenLocked(u.ID) {
				delete(s.Users, u.ID)
			}
		}
	}
	return dropped
}

func (s *ServerSettings) userHasTokenLocked(id string) bool {
	for _, r := range s.Tokens {
		if r.UserID == id {
			return true
		}
	}
	return false
}

// newUserID is "u-" and 8 random bytes of hex.
func newUserID() string { return "u-" + randomID() }

// newUserLocked adds a person with a fresh id.
func (s *ServerSettings) newUserLocked(name, email, avatar, tier, createdBy string) User {
	id := newUserID()
	for _, taken := s.Users[id]; taken; _, taken = s.Users[id] {
		id = newUserID()
	}
	u := User{ID: id, Name: name, Email: email, Avatar: avatar, Tier: tier, CreatedBy: createdBy, CreatedAt: time.Now().Unix()}
	if s.Users == nil {
		s.Users = map[string]User{}
	}
	s.Users[id] = u
	return u
}

// migrate brings an older server.json up to date, idempotently, and reports whether it changed
// anything: expired pairing tokens go, the owner exists, and every credential without a person
// gets its own (limited if it was restricted, else full). Token values are never touched.
func (s *ServerSettings) migrate() bool {
	changed := s.purgeExpiredLocked()
	if s.Users == nil {
		s.Users = map[string]User{}
	}
	if _, ok := s.Users[ownerID]; !ok {
		s.Users[ownerID] = User{ID: ownerID, Tier: TierFull, CreatedAt: time.Now().Unix()}
		changed = true
	}
	for n, r := range s.Tokens {
		if r.UserID != "" && r.Scope == "" {
			continue
		}
		if r.UserID == "" {
			tier := TierFull
			if r.legacyRestricted() {
				tier = TierLimited
			}
			r.UserID = s.newUserLocked(n, r.Email, r.Avatar, tier, "").ID
		}
		r.Scope = ""
		s.Tokens[n] = r
		changed = true
	}
	return changed
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
		out = append(out, TokenSummaryInternal{Name: n, Email: r.Email, HasAvatar: r.Avatar != "", Restricted: s.tierOfLocked(r) != TierFull})
	}
	return out
}

// TokenSummaryInternal mirrors protocol.TokenSummary — kept as a plain
// package-local type so settings.go doesn't need to import internal/protocol
// just for this shape; main.go converts it at the dispatch boundary.
type TokenSummaryInternal struct {
	Name       string
	Email      string
	HasAvatar  bool
	Restricted bool
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

// TokenRestricted reports whether credential name is below the full tier now.
func (s *ServerSettings) TokenRestricted(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	return !ok || s.tierOfLocked(rec) != TierFull
}

// tierOfLocked is min(user tier, device ceiling); a credential whose user is gone is limited.
func (s *ServerSettings) tierOfLocked(rec TokenRecord) string {
	u, ok := s.resolveUserLocked(rec.UserID)
	if !ok {
		return TierLimited
	}
	return minTier(u.Tier, rec.MaxTier)
}

// SetTokenScope is set_peer_scope for old clients: full makes the credential's person full and
// lifts the device ceiling, otherwise the device is held to limited. False if no such credential.
func (s *ServerSettings) SetTokenScope(name string, full bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Tokens[name]
	if !ok {
		return false
	}
	if full {
		if u, ok := s.resolveUserLocked(rec.UserID); ok {
			u.Tier = TierFull
			s.Users[u.ID] = u
		}
		rec.MaxTier = ""
	} else {
		rec.MaxTier = TierLimited
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

// GetLimitAction is the server-wide choice for a usage limit, "ask" unless set.
func (s *ServerSettings) GetLimitAction() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.LimitAction {
	case protocol.LimitActionContinue, protocol.LimitActionStop:
		return s.LimitAction
	}
	return protocol.LimitActionAsk
}

func (s *ServerSettings) SetLimitAction(action string) error {
	switch action {
	case protocol.LimitActionAsk, protocol.LimitActionContinue, protocol.LimitActionStop:
	default:
		return fmt.Errorf("limit action must be ask, continue or stop")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LimitAction = action
	return s.save()
}
