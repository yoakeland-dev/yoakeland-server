package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// A 1x1 png: real image bytes, so the stored picture is read back as an image.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func pngURL() string { return "data:image/png;base64," + tinyPNG }

func newStoreAndSettings(t *testing.T, serverJSON string) (*ServerSettings, *UploadStore, string) {
	t.Helper()
	dir := t.TempDir()
	if serverJSON != "" {
		writeServerJSON(t, dir, serverJSON)
	}
	s, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	up, err := newUploadStore(dir, s.AvatarHashes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.UseAvatarStore(up)
	return s, up, dir
}

func TestSetProfile_PictureGoesToTheBucket(t *testing.T) {
	s, up, _ := newStoreAndSettings(t, "")
	name, pic := "Ada", pngURL()
	prof, err := s.SetProfile(ownerID, &name, &pic)
	if err != nil {
		t.Fatal(err)
	}
	if avatarHash(prof.Avatar) == "" {
		t.Fatalf("avatar = %.40q, want an upload: reference", prof.Avatar)
	}
	if got, ok := up.AvatarDataURL(prof.Avatar); !ok || got != pngURL() {
		t.Fatalf("stored picture didn't round-trip: ok=%v", ok)
	}
	// The same picture again is the same hash, so nothing piles up.
	again, _ := s.SetProfile(ownerID, nil, &pic)
	if again.Avatar != prof.Avatar {
		t.Fatalf("same bytes gave a new reference: %s vs %s", again.Avatar, prof.Avatar)
	}
	emoji := "🦄"
	if p, _ := s.SetProfile(ownerID, nil, &emoji); p.Avatar != emoji {
		t.Fatalf("an emoji must stay as it is, got %q", p.Avatar)
	}
	bad := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte("<script>"))
	if _, err := s.SetProfile(ownerID, nil, &bad); err == nil {
		t.Fatal("a non-image data URL was accepted")
	}
}

// A person's picture is not an orphan: pruning keeps it however old it is.
func TestAvatarSurvivesPrune(t *testing.T) {
	s, up, dir := newStoreAndSettings(t, "")
	pic := pngURL()
	prof, _ := s.SetProfile(ownerID, nil, &pic)
	hash := avatarHash(prof.Avatar)
	old := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "uploads", hash), old, old)
	up.Prune()
	if _, ok := up.AvatarDataURL(prof.Avatar); !ok {
		t.Fatal("prune deleted a picture a person still has")
	}
	// Once nobody has it, it is ordinary garbage.
	empty := ""
	_, _ = s.SetProfile(ownerID, nil, &empty)
	up.Prune()
	if _, ok := up.AvatarDataURL(prof.Avatar); ok {
		t.Fatal("an unreferenced old picture was kept")
	}
}

// The shapes a 0.1.7 server left behind, as they are in a real state dir: a legacy default
// token, a paired phone promoted to Full with its picture as a data URL, an expired pairing code,
// and chats that repeat the author's name and picture on every message.
func TestMigrationFromV017State(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()
	s, up, dir := newStoreAndSettings(t, fmt.Sprintf(`{"identity":"fish","serverId":"abc","tokens":{
		"default":{"token":"legacy"},
		"velly button's phone":{"token":"tp","avatar":%q,"pairing":true,"deviceId":"d1","scope":"full"},
		"Phone":{"token":"tx","pairing":true,"expiresAt":%d},
		"fresh":{"token":"tf","pairing":true,"expiresAt":%d}}}`, pngURL(), past, future))

	if s.Tokens["default"].UserID != ownerID {
		t.Fatalf("default token belongs to %q, want the owner", s.Tokens["default"].UserID)
	}
	if _, ok := s.Tokens["Phone"]; ok {
		t.Fatal("an expired pairing code survived")
	}
	if s.Tokens["fresh"].UserID == "" || s.Users[s.Tokens["fresh"].UserID].Tier != TierLimited {
		t.Fatal("an unclaimed pairing code should be a Limited person")
	}
	phone := s.Users[s.Tokens["velly button's phone"].UserID]
	if phone.Tier != TierFull {
		t.Fatalf("a Full-scope phone became %s", phone.Tier)
	}
	if avatarHash(phone.Avatar) == "" {
		t.Fatalf("the phone's data-URL picture should be in the bucket, got %.30q", phone.Avatar)
	}
	if got, ok := up.AvatarDataURL(phone.Avatar); !ok || got != pngURL() {
		t.Fatal("the migrated picture can't be read back")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "server.json"))
	if strings.Contains(string(raw), `"scope"`) {
		t.Fatal("server.json still holds a scope")
	}
	for id, u := range s.Users {
		if strings.HasPrefix(u.Avatar, "data:") {
			t.Fatalf("person %s still keeps a data URL", id)
		}
	}

	// Old messages say "velly button", a name nobody has now, with the picture on each one.
	old := []protocol.HistoryMessage{
		{ID: "m1", Author: "velly button", AuthorAvatar: pngURL()},
		{ID: "m2", Author: "stranger", AuthorAvatar: "data:image/png;base64,AAAA"},
		{ID: "m3", Author: "someone", AuthorAvatar: "🦄"},
	}
	users, _, _ := s.view()
	profiles := newAuthorIndex(users).apply(old)
	if old[0].AuthorID != phone.ID || old[0].AuthorAvatar != "" {
		t.Fatalf("old message not joined to its person by picture: %+v", old[0])
	}
	if profiles[phone.ID].Avatar != phone.Avatar {
		t.Fatalf("profile picture = %q", profiles[phone.ID].Avatar)
	}
	if old[1].AuthorID != "" || old[1].AuthorAvatar == "" {
		t.Fatalf("an unknown picture must leave the message alone: %+v", old[1])
	}
	if old[2].AuthorID != "" {
		t.Fatalf("an emoji must not place anyone: %+v", old[2])
	}

	// A second start changes nothing.
	again, err := NewServerSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.UseAvatarStore(up)
	if len(again.Users) != len(s.Users) || again.Users[phone.ID].Avatar != phone.Avatar || again.Tokens["default"].UserID != ownerID {
		t.Fatal("restarting changed the migrated state")
	}
}
