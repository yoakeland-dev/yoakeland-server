package main

import (
	"bytes"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// A device chooses its own display name, so nothing that attributes an action may use it:
// commits are signed with, and the audit log blames, the token's name.
func TestPairedDeviceIdentity(t *testing.T) {
	deps := newTestDeps(t)
	p := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), &fakeCloser{}, "tcp", peerIdentity{Name: "aveline", TokenName: "fish's phone", Email: "fish@example.com"})

	if got := actorOf(p); got != "fish's phone" {
		t.Fatalf("audit actor = %q, want the credential name", got)
	}
	a := resolveAuthor(deps, p.ID, "", "")
	if a.Name != "aveline" || a.GitName() != "fish's phone" || a.Email != "fish@example.com" {
		t.Fatalf("author = %+v (git %q), want display name aveline but commits as the credential", a, a.GitName())
	}
	deps.peers.Rename(p.ID, "someone else")
	if a = resolveAuthor(deps, p.ID, "", ""); a.Name != "someone else" || a.GitName() != "fish's phone" {
		t.Fatalf("after rename author = %+v, want the new display name and the same git name", a)
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
