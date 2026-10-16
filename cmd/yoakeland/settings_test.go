package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
