package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/protocol"
)

// NET-16: an unparseable server.json is set aside, not overwritten by the
// save that mints a fresh ServerID.
func TestServerSettings_CorruptFileSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServerSettings(dir); err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("expected one set-aside copy, got %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "{not json" {
		t.Fatalf("set-aside copy = %q, want the original bytes", b)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "serverId") {
		t.Fatalf("server.json was not rewritten with a fresh ServerID: %q", b)
	}
}

// TestServerSettings_DefaultsIdentityToHostname guards that a fresh server
// (no server.json yet) still has a non-empty display name to show peers.
func TestServerSettings_DefaultsIdentityToHostname(t *testing.T) {
	s, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	if s.GetIdentity() == "" {
		t.Fatal("expected a non-empty default identity")
	}
}

// TestServerSettings_ServerIDIsStableAndUnique guards the whole point of
// ServerID: two independent state dirs get different ids, and reopening the
// same dir returns the exact same one rather than minting a new one every
// launch (which would defeat its purpose as a stable cross-reconnect key).
func TestServerSettings_ServerIDIsStableAndUnique(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()

	a, err := NewServerSettings(dirA)
	if err != nil {
		t.Fatalf("NewServerSettings(dirA): %v", err)
	}
	b, err := NewServerSettings(dirB)
	if err != nil {
		t.Fatalf("NewServerSettings(dirB): %v", err)
	}
	idA, idB := a.GetServerID(), b.GetServerID()
	if idA == "" || idB == "" {
		t.Fatalf("expected non-empty server ids, got %q and %q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("expected two fresh state dirs to get different server ids, both got %q", idA)
	}

	reopened, err := NewServerSettings(dirA)
	if err != nil {
		t.Fatalf("reopening NewServerSettings(dirA): %v", err)
	}
	if got := reopened.GetServerID(); got != idA {
		t.Fatalf("GetServerID() after reopen = %q, want the original %q", got, idA)
	}
}

// TestServerSettings_PersistsAcrossReopen guards the actual point of this
// store: set_server_identity/set_remote_password must survive a restart,
// not reset to flag defaults every time (the whole reason they're here
// instead of just in-memory).
func TestServerSettings_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	if err := s.SetIdentity("aveline-server"); err != nil {
		t.Fatalf("SetIdentity: %v", err)
	}
	if err := s.SetToken("rotated-password"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	reopened, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("reopening NewServerSettings: %v", err)
	}
	if got := reopened.GetIdentity(); got != "aveline-server" {
		t.Fatalf("GetIdentity() after reopen = %q, want %q", got, "aveline-server")
	}
	if got := reopened.GetToken(); got != "rotated-password" {
		t.Fatalf("GetToken() after reopen = %q, want %q", got, "rotated-password")
	}
}

// TestServerSettings_AutoGenerateTitleDefaultsOnAndPersists guards the
// nil-means-true default (a fresh server.json has no autoGenerateTitle key
// at all) and that an explicit false survives a restart the same way
// identity/token do.
func TestServerSettings_AutoGenerateTitleDefaultsOnAndPersists(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	if !s.GetAutoGenerateTitle() {
		t.Fatal("expected auto-generate-title to default to true")
	}
	if err := s.SetAutoGenerateTitle(false); err != nil {
		t.Fatalf("SetAutoGenerateTitle: %v", err)
	}

	reopened, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("reopening NewServerSettings: %v", err)
	}
	if reopened.GetAutoGenerateTitle() {
		t.Fatal("expected auto-generate-title=false to survive reopen")
	}
}

// TestServerSettings_NamedTokenRoundTrip guards register_token/list_tokens/
// remove_token's backing store: a named token resolves back to its own
// name/email via LookupToken, shows up (scrubbed) in ListTokenNames, its
// avatar persists via SetTokenAvatar, and removing it is idempotent.
func TestServerSettings_NamedTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}

	if err := s.AddToken("aveline", "tok-123", "aveline@example.com"); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	if err := s.AddToken("aveline", "tok-456", ""); err == nil {
		t.Fatal("expected AddToken to reject a duplicate name")
	}

	name, rec, ok := s.LookupToken("tok-123")
	if !ok || name != "aveline" || rec.Email != "aveline@example.com" {
		t.Fatalf("LookupToken(tok-123) = name=%q rec=%+v ok=%v, want aveline/aveline@example.com/true", name, rec, ok)
	}
	if _, _, ok := s.LookupToken("wrong"); ok {
		t.Fatal("LookupToken should not match an unregistered token")
	}

	if !s.SetTokenAvatar("aveline", "data:image/png;base64,abc") {
		t.Fatal("SetTokenAvatar should report the token existed")
	}

	// Persistence must survive a reopen, not just live in memory.
	reopened, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("reopening NewServerSettings: %v", err)
	}
	summaries := reopened.ListTokenNames()
	if len(summaries) != 1 || summaries[0].Name != "aveline" || !summaries[0].HasAvatar {
		t.Fatalf("ListTokenNames() after reopen = %+v, want one scrubbed entry for aveline with an avatar", summaries)
	}
	if _, rec, ok := reopened.LookupToken("tok-123"); !ok || rec.Avatar == "" {
		t.Fatalf("expected the persisted avatar to survive reopen, got rec=%+v ok=%v", rec, ok)
	}

	existed, err := reopened.RemoveToken("aveline")
	if err != nil || !existed {
		t.Fatalf("RemoveToken(aveline) = existed=%v err=%v, want true/nil", existed, err)
	}
	existed, err = reopened.RemoveToken("aveline")
	if err != nil || existed {
		t.Fatalf("RemoveToken(aveline) second call = existed=%v err=%v, want false/nil (idempotent)", existed, err)
	}
}

// TestServerSettings_LegacyTokenMigratesToDefault guards that a
// pre-multi-token server.json (bare "token" field) still round-trips
// through GetToken/SetToken exactly as before — a single-user setup needs
// zero config changes.
func TestServerSettings_LegacyTokenMigratesToDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	if err := s.SetToken("legacy-secret"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	reopened, err := NewServerSettings(dir)
	if err != nil {
		t.Fatalf("reopening NewServerSettings: %v", err)
	}
	if got := reopened.GetToken(); got != "legacy-secret" {
		t.Fatalf("GetToken() after reopen = %q, want %q", got, "legacy-secret")
	}
	if name, _, ok := reopened.LookupToken("legacy-secret"); !ok || name != defaultTokenName {
		t.Fatalf("LookupToken(legacy-secret) = name=%q ok=%v, want %q/true", name, ok, defaultTokenName)
	}
}

// TestClaimPairing_RemovedTokenIsRefused guards NET-10: a pairing token removed while its
// device was waiting for approval must not be admitted by ClaimPairing.
func TestClaimPairing_RemovedTokenIsRefused(t *testing.T) {
	s, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatalf("NewServerSettings: %v", err)
	}
	if err := s.AddPairingToken("phone", "tok-pair", ""); err != nil {
		t.Fatalf("AddPairingToken: %v", err)
	}
	if _, err := s.RemoveToken("phone"); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}
	if err := s.ClaimPairing("phone", "device-1"); err == nil {
		t.Fatal("ClaimPairing on a removed token should fail")
	}
}

func TestServerSettings_Icon(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := 330
	if err := s.SetIcon(protocol.ServerIcon{Name: "ghost", Hue: &h}); err != nil {
		t.Fatal(err)
	}
	re, _ := NewServerSettings(dir)
	if ic := re.GetIcon(); ic == nil || ic.Name != "ghost" || ic.Hue == nil || *ic.Hue != 330 {
		t.Fatalf("icon after reopen = %+v", ic)
	}
	bad := 360
	for _, ic := range []protocol.ServerIcon{{Name: "Ghost"}, {Name: "a b"}, {Name: strings.Repeat("a", 33)}, {Hue: &bad}} {
		if err := re.SetIcon(ic); err == nil {
			t.Errorf("SetIcon(%+v) accepted", ic)
		}
	}
	if re.GetIcon() == nil {
		t.Fatal("a rejected icon must leave the stored one alone")
	}
	if err := re.SetIcon(protocol.ServerIcon{}); err != nil || re.GetIcon() != nil {
		t.Fatalf("empty icon should clear, got %+v (%v)", re.GetIcon(), err)
	}
}

func writeServerJSON(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "server.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An old server.json (tokens with a Scope, no users) gains the owner and one person per
// credential, and loading it again changes nothing.
func TestServerSettings_MigratesToUsers(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()
	writeServerJSON(t, dir, fmt.Sprintf(`{"serverId":"abc","token":"legacy-secret","tokens":{
		"alice":{"token":"ta","email":"a@x.io","avatar":"😀"},
		"phone":{"token":"tp","pairing":true,"expiresAt":%d,"deviceId":"d1"},
		"promoted":{"token":"tq","pairing":true,"deviceId":"d2","scope":"full"},
		"demoted":{"token":"tr","scope":"limited"},
		"fresh":{"token":"tf","pairing":true,"expiresAt":%d},
		"stale":{"token":"ts","pairing":true,"expiresAt":%d}}}`, future, future, past))

	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := s.Users[ownerID]; !ok || owner.Tier != TierFull {
		t.Fatalf("owner = %+v, %v", owner, ok)
	}
	if _, ok := s.Tokens["stale"]; ok {
		t.Fatal("an expired pairing token survived the load")
	}
	want := map[string]string{"alice": TierFull, "default": TierFull, "phone": TierLimited, "promoted": TierFull, "demoted": TierLimited, "fresh": TierLimited}
	for name, tier := range want {
		rec, ok := s.Tokens[name]
		if !ok {
			t.Fatalf("token %q missing", name)
		}
		u, ok := s.Users[rec.UserID]
		if !ok || u.Name != name || u.Tier != tier {
			t.Fatalf("token %q -> user %+v (found %v), want name %q tier %s", name, u, ok, name, tier)
		}
		if rec.Scope != "" {
			t.Fatalf("token %q kept Scope %q", name, rec.Scope)
		}
	}
	if a := s.Users[s.Tokens["alice"].UserID]; a.Email != "a@x.io" || a.Avatar != "😀" {
		t.Fatalf("alice's email and avatar didn't move to her user: %+v", a)
	}
	if s.Tokens["alice"].Token != "ta" || s.Tokens["default"].Token != "legacy-secret" {
		t.Fatal("a token value changed")
	}

	// Loading the migrated file again is a no-op: same users, same links.
	before, _ := json.Marshal(s)
	again, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(again)
	if string(before) != string(after) {
		t.Fatalf("second load changed the file:\n%s\n%s", before, after)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "server.json"))
	if strings.Contains(string(raw), `"scope"`) {
		t.Fatal("scope is still written to server.json")
	}
	if len(again.Users) != len(want)+1 {
		t.Fatalf("users = %d, want %d (credentials + owner)", len(again.Users), len(want)+1)
	}
}

func TestServerSettings_PurgesExpiredPairingOnLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPairingToken("gone", "tg", ""); err != nil {
		t.Fatal(err)
	}
	userID := s.Tokens["gone"].UserID
	rec := s.Tokens["gone"]
	rec.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	s.Tokens["gone"] = rec
	u := s.Users[userID]
	u.CreatedAt = rec.ExpiresAt - int64(PairingTTL/time.Second)
	s.Users[userID] = u
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	again, _ := NewServerSettings(dir)
	if _, ok := again.Tokens["gone"]; ok {
		t.Fatal("an expired unclaimed pairing token was kept on load")
	}
	if _, ok := again.Users[userID]; ok {
		t.Fatal("the person made for that code was kept")
	}
}

func TestServerSettings_SeedOwnerProfileOnce(t *testing.T) {
	s, _ := NewServerSettings(t.TempDir())
	s.SeedOwnerProfile("Ava", "🦊")
	s.SeedOwnerProfile("Someone else", "🐻")
	if o := s.Users[ownerID]; o.Name != "Ava" || o.Avatar != "🦊" {
		t.Fatalf("owner = %+v, want the first report kept", o)
	}
}
