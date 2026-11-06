package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// Revoking access by event, as the owner's client does it, with turns running: what stops, what
// stays, and what an old connection can still do.

type revokeWorld struct {
	deps   connDeps
	block  *blockingModel
	root   string
	closed map[string]*atomic.Bool // by peer id
}

func newRevokeWorld(t *testing.T) *revokeWorld {
	t.Helper()
	block := &blockingModel{started: make(chan struct{}, 16)}
	projects, err := NewProjectRegistry(block, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := projects.Get(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := connDeps{projects: projects, defaultProjectRoot: root, peers: NewPeerRegistry(), settings: cfg, turnWG: &sync.WaitGroup{}}
	return &revokeWorld{deps: deps, block: block, root: root, closed: map[string]*atomic.Bool{}}
}

// device connects a device for userID (a new person when empty) and records when it is closed.
func (w *revokeWorld) device(t *testing.T, userID, tier string) *Peer {
	t.Helper()
	s := w.deps.settings
	s.mu.Lock()
	if userID == "" {
		userID = s.newUserLocked("person", "", "", tier, ownerID).ID
	}
	cred := "cred-" + randomID()
	s.Tokens[cred] = TokenRecord{Token: "tok-" + cred, UserID: userID}
	s.mu.Unlock()
	gone := &atomic.Bool{}
	p := w.deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard), closeFn(func() { gone.Store(true) }), "tcp",
		peerIdentity{Name: userID, TokenName: cred, Restricted: tier != TierFull, Tier: tier, UserID: userID})
	w.closed[p.ID] = gone
	return p
}

// prompt starts a turn from p in a new chat and waits until the model has it.
func (w *revokeWorld) prompt(t *testing.T, p *Peer, session string) {
	t.Helper()
	raw, _ := json.Marshal(protocol.UserPromptData{Prompt: "go", SessionID: session})
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
	handleEnvelope(context.Background(), ipc, w.deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventUserPrompt, SessionID: session, TurnID: "t-" + session, Data: raw})
	select {
	case <-w.block.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("the turn in %s never started", session)
	}
}

// active waits for exactly n running turns.
func (w *revokeWorld) active(t *testing.T, n int, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a, _ := w.deps.projects.Stats(); a == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, _ := w.deps.projects.Stats()
	t.Fatalf("%s: %d turns running, want %d", why, a, n)
}

func (w *revokeWorld) owner(t *testing.T, typ protocol.EventType, body string) string {
	t.Helper()
	return send(t, w.deps, "unix", nil, typ, body)
}

func (w *revokeWorld) wasClosed(p *Peer) bool { return w.closed[p.ID].Load() }

// Revoking one device stops what it started and drops it, and leaves the person's other device,
// and everyone else, alone.
func TestRevokeDevice_StopsItsTurnsOnly(t *testing.T) {
	w := newRevokeWorld(t)
	phone := w.device(t, "", TierTrusted)
	laptop := w.device(t, phone.UserID, TierTrusted)
	other := w.device(t, "", TierTrusted)
	w.prompt(t, phone, "s-phone")
	w.prompt(t, laptop, "s-laptop")
	w.prompt(t, other, "s-other")
	w.active(t, 3, "before")
	personID, laptopCred := phone.UserID, laptop.TokenName // a revoked peer's own record is cleared

	if out := w.owner(t, protocol.EventRemoveToken, `{"name":"`+phone.TokenName+`"}`); refused(out) {
		t.Fatalf("remove_token refused: %s", out)
	}
	w.active(t, 2, "after revoking the phone")
	if !w.wasClosed(phone) {
		t.Fatal("the revoked device stayed connected")
	}
	if w.wasClosed(laptop) || w.wasClosed(other) {
		t.Fatal("revoking one device dropped another")
	}
	if _, ok := w.deps.settings.TokenRecordOf(phone.TokenName); ok {
		t.Fatal("the revoked credential is still stored")
	}
	if _, ok := w.deps.settings.GetUser(personID); !ok {
		t.Fatal("revoking a device removed the person")
	}
	if got := w.deps.settings.Access("tcp", laptopCred); got.Tier != TierTrusted {
		t.Fatalf("the person's other device lost access: %s", got.Tier)
	}
}

// Removing a person revokes every device they have and stops every turn they started.
func TestRemovePerson_RevokesEverythingTheyHad(t *testing.T) {
	w := newRevokeWorld(t)
	phone := w.device(t, "", TierTrusted)
	laptop := w.device(t, phone.UserID, TierTrusted)
	other := w.device(t, "", TierTrusted)
	w.prompt(t, phone, "s-phone")
	w.prompt(t, laptop, "s-laptop")
	w.prompt(t, other, "s-other")
	w.active(t, 3, "before")
	personID, creds := phone.UserID, []string{phone.TokenName, laptop.TokenName}

	if out := w.owner(t, protocol.EventRemoveUser, `{"userId":"`+personID+`"}`); refused(out) {
		t.Fatalf("remove_user refused: %s", out)
	}
	w.active(t, 1, "after removing the person")
	if !w.wasClosed(phone) || !w.wasClosed(laptop) {
		t.Fatal("a removed person's device stayed connected")
	}
	if w.wasClosed(other) {
		t.Fatal("removing a person dropped someone else's device")
	}
	for _, c := range creds {
		if _, ok := w.deps.settings.TokenRecordOf(c); ok {
			t.Fatalf("credential %s survived the person", c)
		}
	}
	if _, ok := w.deps.settings.GetUser(personID); ok {
		t.Fatal("the person is still stored")
	}
}

// Lowering access applies at once, by event too: the tier on the person, or the ceiling on one device.
func TestLowerAccessByEvent_StopsOnlyWhatItCovers(t *testing.T) {
	w := newRevokeWorld(t)
	phone := w.device(t, "", TierTrusted)
	laptop := w.device(t, phone.UserID, TierTrusted)
	other := w.device(t, "", TierTrusted)
	w.prompt(t, phone, "s-phone")
	w.prompt(t, laptop, "s-laptop")
	w.prompt(t, other, "s-other")
	w.active(t, 3, "before")
	personID, phoneCred, laptopCred, otherCred := phone.UserID, phone.TokenName, laptop.TokenName, other.TokenName

	// A ceiling on the phone stops the phone's turn and nothing else.
	if out := w.owner(t, protocol.EventSetDeviceRestrict, `{"credential":"`+phoneCred+`","maxTier":"limited"}`); refused(out) {
		t.Fatalf("set_device_restrictions refused: %s", out)
	}
	w.active(t, 2, "after capping the phone")
	if got := w.deps.settings.Access("tcp", phoneCred).Tier; got != TierLimited {
		t.Fatalf("capped phone is %s", got)
	}
	if got := w.deps.settings.Access("tcp", laptopCred).Tier; got != TierTrusted {
		t.Fatalf("the person's other device was lowered too: %s", got)
	}

	// Lowering the person lowers every device they have and stops the rest of their turns.
	if out := w.owner(t, protocol.EventSetUserPermissions, `{"userId":"`+personID+`","tier":"limited"}`); refused(out) {
		t.Fatalf("set_user_permissions refused: %s", out)
	}
	w.active(t, 1, "after lowering the person")
	if got := w.deps.settings.Access("tcp", laptopCred).Tier; got != TierLimited {
		t.Fatalf("the laptop is still %s", got)
	}
	if p, _ := w.deps.peers.Get(laptop.ID); p.Tier != TierLimited {
		t.Fatalf("the live connection still says %s", p.Tier)
	}
	if got := w.deps.settings.Access("tcp", otherCred).Tier; got != TierTrusted {
		t.Fatalf("a bystander was lowered: %s", got)
	}
}

// A device that is offline is listed and can be revoked, and its credential stops working.
func TestOfflineDeviceCanBeRevoked(t *testing.T) {
	w := newRevokeWorld(t)
	s := w.deps.settings
	s.mu.Lock()
	u := s.newUserLocked("away", "", "", TierTrusted, ownerID)
	s.Tokens["old-laptop"] = TokenRecord{Token: "tok-old", UserID: u.ID}
	s.mu.Unlock()

	listed := w.owner(t, protocol.EventListPeople, `{}`)
	if !strings.Contains(listed, `"credential":"old-laptop"`) || strings.Contains(listed, `"credential":"old-laptop","online":true`) {
		t.Fatalf("an offline device should be listed as offline: %s", listed)
	}
	if _, _, ok := s.LookupToken("tok-old"); !ok {
		t.Fatal("the credential should work before it's revoked")
	}
	if out := w.owner(t, protocol.EventRemoveToken, `{"name":"old-laptop"}`); refused(out) {
		t.Fatalf("revoking an offline device failed: %s", out)
	}
	if _, _, ok := s.LookupToken("tok-old"); ok {
		t.Fatal("a revoked credential still authenticates")
	}
	if got := w.owner(t, protocol.EventListPeople, `{}`); strings.Contains(got, "old-laptop") {
		t.Fatalf("the revoked device is still listed: %s", got)
	}
	if got := s.Access("tcp", "old-laptop"); got.Tier != TierLimited || got.Caps.Has(CapFilesEdit) {
		t.Fatalf("an unknown credential must have nothing, got %+v", got)
	}
}

// A connection that outlives its revocation can do nothing: its access is read on every request.
func TestRevokedConnectionIsRefusedEverything(t *testing.T) {
	w := newRevokeWorld(t)
	p := w.device(t, "", TierFull)
	personID := p.UserID
	w.owner(t, protocol.EventRemoveToken, `{"name":"`+p.TokenName+`"}`)

	for _, ev := range []struct {
		typ  protocol.EventType
		body string
	}{
		{protocol.EventRestartServer, `{}`},
		{protocol.EventListPeople, `{}`},
		{protocol.EventSetUserPermissions, `{"userId":"` + personID + `","tier":"full"}`},
		{protocol.EventWriteFile, `{"path":"x","content":"y"}`},
		{protocol.EventSaveMCPServer, `{}`},
	} {
		if out := send(t, w.deps, "tcp", p, ev.typ, ev.body); !refused(out) {
			t.Errorf("%s was answered for a revoked connection: %.120s", ev.typ, out)
		}
	}
}
