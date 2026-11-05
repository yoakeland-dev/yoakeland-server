package main

import (
	"bytes"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// A device chooses its own display name and picture, so neither may attribute anything: messages
// and commits name the person behind the credential, and the audit log blames the credential.
func TestPairedDeviceIdentity(t *testing.T) {
	deps := newTestDeps(t)
	deps.settings.mu.Lock()
	u := deps.settings.newUserLocked("Fish", "fish@example.com", "🐟", TierLimited, ownerID)
	deps.settings.Tokens["fish's phone"] = TokenRecord{Token: "tok", UserID: u.ID}
	deps.settings.mu.Unlock()
	p := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), &fakeCloser{}, "tcp",
		peerIdentity{Name: "aveline", Avatar: "🐣", TokenName: "fish's phone", Tier: TierLimited, UserID: u.ID})

	if got := actorOf(p); got != "fish's phone" {
		t.Fatalf("audit actor = %q, want the credential name", got)
	}
	a := resolveAuthor(deps, p.ID, "claims to be root")
	if a.ID != u.ID || a.Name != "Fish" || a.GitName() != "Fish" || a.Email != "fish@example.com" || a.Avatar != "🐟" {
		t.Fatalf("author = %+v (git %q), want the person behind the credential", a, a.GitName())
	}
	// Renaming or re-picturing the device changes the device and nothing about the person.
	deps.peers.Rename(p.ID, "someone else")
	deps.peers.SetAvatar(p.ID, "🦊")
	send(t, deps, "tcp", p, protocol.EventSetPeerName, `{"name":"root"}`)
	send(t, deps, "tcp", p, protocol.EventSetPeerAvatar, `{"avatar":"🦖"}`)
	if b := resolveAuthor(deps, p.ID, ""); b != a {
		t.Fatalf("after relabelling the device author = %+v, want %+v", b, a)
	}
	if got, _ := deps.settings.GetUser(u.ID); got.Name != "Fish" || got.Avatar != "🐟" || len(got.Names) != 0 {
		t.Fatalf("the person changed with the device: %+v", got)
	}
}

// The local socket is the owner; the name a fresh install's client reports is used only while the
// owner has none.
func TestLocalSocketAuthorIsTheOwner(t *testing.T) {
	deps := newTestDeps(t)
	local := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), &fakeCloser{}, "unix", peerIdentity{Name: "my laptop"})
	if a := resolveAuthor(deps, local.ID, "Ada"); a.ID != ownerID || a.Name != "Ada" {
		t.Fatalf("unseeded owner: %+v", a)
	}
	deps.settings.SeedOwnerProfile("Ada Lovelace", "🧮")
	if a := resolveAuthor(deps, local.ID, "someone"); a.ID != ownerID || a.Name != "Ada Lovelace" || a.Avatar != "🧮" {
		t.Fatalf("seeded owner: %+v", a)
	}
	if a := resolveAuthor(deps, "", ""); a.ID != ownerID {
		t.Fatalf("a connection that isn't a peer (-stdio): %+v", a)
	}
}

func TestCleanName(t *testing.T) {
	if got := cleanName("  fi\x00sh\n "); got != "fish" {
		t.Fatalf("cleanName = %q, want control characters and padding removed", got)
	}
	if got := cleanName(strings.Repeat("é", 200)); len([]rune(got)) != maxNameLen {
		t.Fatalf("cleanName kept %d runes, want %d", len([]rune(got)), maxNameLen)
	}
}
