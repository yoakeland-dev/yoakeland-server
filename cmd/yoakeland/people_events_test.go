package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// send runs one envelope from a connection and returns everything it wrote back.
func send(t *testing.T, deps connDeps, network string, from *Peer, typ protocol.EventType, data string) string {
	t.Helper()
	var out syncBuf
	id := ""
	if from != nil {
		id = from.ID
	}
	handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &out), deps, network, id, &protocol.Envelope{Type: typ, Data: json.RawMessage(data)})
	return out.String()
}

func refused(out string) bool { return strings.Contains(out, `"type":"error"`) }

// registerToken sends register_token and returns the answer.
func registerToken(t *testing.T, deps connDeps, network string, from *Peer, data string) string {
	return send(t, deps, network, from, protocol.EventRegisterToken, data)
}

func userOfToken(t *testing.T, deps connDeps, cred string) (User, TokenRecord) {
	t.Helper()
	rec, ok := deps.settings.TokenRecordOf(cred)
	if !ok {
		t.Fatalf("credential %q not stored", cred)
	}
	u, ok := deps.settings.ResolveUser(rec.UserID)
	if !ok {
		t.Fatalf("credential %q has no person", cred)
	}
	return u, rec
}

// Who may pair a device for whom, and at which tier (PLAN_PERMISSIONS "Pairing").
func TestRegisterToken_Matrix(t *testing.T) {
	deps := newTestDeps(t)
	full := addPerson(t, deps, TierFull)
	trusted := addPerson(t, deps, TierTrusted)
	limited := addPerson(t, deps, TierLimited)
	existing, _ := deps.settings.CreateUser("Ada", "", TierTrusted, ownerID)

	type tc struct {
		name    string
		network string
		from    *Peer
		body    string
		ok      bool
		check   func(t *testing.T, u User, rec TokenRecord)
	}
	cases := []tc{
		{"local: me", "unix", nil, `{"name":"c1","pairing":true,"user":"me"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.ID != ownerID || r.MaxTier != TierLimited {
				t.Errorf("user=%s maxTier=%q, want owner and a limited device", u.ID, r.MaxTier)
			}
		}},
		{"local: me with full", "unix", nil, `{"name":"c2","pairing":true,"user":"me","full":true}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.ID != ownerID || r.MaxTier != "" {
				t.Errorf("user=%s maxTier=%q, want owner and no ceiling", u.ID, r.MaxTier)
			}
		}},
		{"local: me with an explicit ceiling", "unix", nil, `{"name":"c3","pairing":true,"user":"me","maxTier":"trusted"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if r.MaxTier != TierTrusted {
				t.Errorf("maxTier=%q", r.MaxTier)
			}
		}},
		{"local: new trusted person", "unix", nil, `{"name":"c4","pairing":true,"user":"new","newUserName":"Bea","tier":"trusted"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Name != "Bea" || u.Tier != TierTrusted || u.CreatedBy != ownerID || r.PairedBy != ownerID {
				t.Errorf("user=%+v rec=%+v", u, r)
			}
		}},
		{"local: an existing person", "unix", nil, `{"name":"c5","pairing":true,"user":"` + existing.ID + `"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.ID != existing.ID {
				t.Errorf("user=%s", u.ID)
			}
		}},
		{"local: an unknown person", "unix", nil, `{"name":"c6","pairing":true,"user":"u-nobody"}`, false, nil},
		{"old client pairing: new, named after the token, limited", "unix", nil, `{"name":"c7","pairing":true}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Name != "c7" || u.Tier != TierLimited {
				t.Errorf("user=%+v", u)
			}
		}},
		{"old client pairing with full", "unix", nil, `{"name":"c8","pairing":true,"full":true}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Tier != TierFull {
				t.Errorf("user=%+v", u)
			}
		}},
		{"full: me is themselves", "tcp", full, `{"name":"f1","pairing":true,"user":"me"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.ID != full.UserID || r.PairedBy != full.UserID {
				t.Errorf("user=%s pairedBy=%s", u.ID, r.PairedBy)
			}
		}},
		{"full: new full person", "tcp", full, `{"name":"f2","pairing":true,"user":"new","tier":"full"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Tier != TierFull || u.CreatedBy != full.UserID {
				t.Errorf("user=%+v", u)
			}
		}},
		{"full: an existing person", "tcp", full, `{"name":"f3","pairing":true,"user":"` + existing.ID + `"}`, false, nil},
		{"trusted: new limited person", "tcp", trusted, `{"name":"t1","pairing":true,"user":"new","newUserName":"Cy","tier":"limited"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Tier != TierLimited || u.CreatedBy != trusted.UserID || r.PairedBy != trusted.UserID {
				t.Errorf("user=%+v rec=%+v", u, r)
			}
		}},
		{"trusted: a pairing with no user is a new limited person", "tcp", trusted, `{"name":"t2","pairing":true}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Tier != TierLimited {
				t.Errorf("user=%+v", u)
			}
		}},
		{"trusted: a plain token is limited too", "tcp", trusted, `{"name":"t3"}`, true, func(t *testing.T, u User, r TokenRecord) {
			if u.Tier != TierLimited {
				t.Errorf("user=%+v", u)
			}
		}},
		{"trusted cannot create a Full user", "tcp", trusted, `{"name":"t4","pairing":true,"user":"new","tier":"full"}`, false, nil},
		{"trusted cannot create a Full user with the old flag", "tcp", trusted, `{"name":"t5","pairing":true,"full":true}`, false, nil},
		{"trusted cannot create a Trusted user", "tcp", trusted, `{"name":"t6","pairing":true,"user":"new","tier":"trusted"}`, false, nil},
		{"trusted: me", "tcp", trusted, `{"name":"t7","pairing":true,"user":"me"}`, false, nil},
		{"trusted: an existing person", "tcp", trusted, `{"name":"t8","pairing":true,"user":"` + existing.ID + `"}`, false, nil},
		{"limited cannot pair", "tcp", limited, `{"name":"l1","pairing":true,"user":"new"}`, false, nil},
		{"limited cannot pair the old way either", "tcp", limited, `{"name":"l2","pairing":true}`, false, nil},
		{"a bad tier", "unix", nil, `{"name":"b1","pairing":true,"user":"new","tier":"wizard"}`, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var name struct{ Name string }
			_ = json.Unmarshal([]byte(c.body), &name)
			out := registerToken(t, deps, c.network, c.from, c.body)
			if c.ok {
				if refused(out) || !strings.Contains(out, `"token_registered"`) {
					t.Fatalf("refused: %s", out)
				}
				u, rec := userOfToken(t, deps, name.Name)
				if c.check != nil {
					c.check(t, u, rec)
				}
				return
			}
			if !refused(out) {
				t.Fatalf("was allowed: %s", out)
			}
			if _, ok := deps.settings.TokenRecordOf(name.Name); ok {
				t.Fatalf("a refused request still made credential %q", name.Name)
			}
		})
	}
}

// A refused request leaves no person behind, and a name that is taken fails before making one.
func TestRegisterToken_NoOrphanOnFailure(t *testing.T) {
	deps := newTestDeps(t)
	registerToken(t, deps, "unix", nil, `{"name":"dup","pairing":true}`)
	n := len(deps.settings.Users)
	if out := registerToken(t, deps, "unix", nil, `{"name":"dup","pairing":true}`); !refused(out) {
		t.Fatal("a duplicate name was accepted")
	}
	if len(deps.settings.Users) != n {
		t.Fatal("a failed registration left a person behind")
	}
}

// A Trusted person sees and removes only the people they added, and themselves.
func TestTrustedManagesOnlyThePeopleTheyAdded(t *testing.T) {
	deps := newTestDeps(t)
	var out syncBuf
	tess := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &out))
	other := addPerson(t, deps, TierTrusted)
	registerToken(t, deps, "tcp", tess, `{"name":"mine","pairing":true,"user":"new","newUserName":"Mine"}`)
	registerToken(t, deps, "unix", nil, `{"name":"owners","pairing":true,"user":"new","newUserName":"Owners"}`)
	registerToken(t, deps, "tcp", other, `{"name":"theirs","pairing":true,"user":"new","newUserName":"Theirs"}`)

	people := send(t, deps, "tcp", tess, protocol.EventListPeople, `{}`)
	for _, want := range []string{`"name":"Mine"`, tess.UserID} {
		if !strings.Contains(people, want) {
			t.Errorf("people lacks %s: %s", want, people)
		}
	}
	for _, not := range []string{`"name":"Owners"`, `"name":"Theirs"`, other.UserID, `"id":"owner"`} {
		if strings.Contains(people, not) {
			t.Errorf("a Trusted person was shown %s: %s", not, people)
		}
	}
	tokens := send(t, deps, "tcp", tess, protocol.EventListTokens, `{}`)
	if !strings.Contains(tokens, `"name":"mine"`) || !strings.Contains(tokens, tess.TokenName) || strings.Contains(tokens, `"name":"owners"`) || strings.Contains(tokens, `"name":"theirs"`) {
		t.Errorf("list_tokens: %s", tokens)
	}
	if strings.Contains(people, "tok-") || strings.Contains(tokens, "tok-") {
		t.Error("a token value leaked")
	}

	if got := send(t, deps, "tcp", tess, protocol.EventRemoveToken, `{"name":"theirs"}`); !refused(got) {
		t.Errorf("removed someone else's person's credential: %s", got)
	}
	if got := send(t, deps, "tcp", tess, protocol.EventRemoveToken, `{"name":"owners"}`); !refused(got) {
		t.Errorf("removed the owner's credential: %s", got)
	}
	if _, ok := deps.settings.TokenRecordOf("theirs"); !ok {
		t.Fatal("the credential was removed anyway")
	}
	if got := send(t, deps, "tcp", tess, protocol.EventRemoveToken, `{"name":"mine"}`); refused(got) {
		t.Errorf("couldn't remove a credential of someone they added: %s", got)
	}
	if _, ok := deps.settings.TokenRecordOf("mine"); ok {
		t.Fatal("their own person's credential survived")
	}
	// A Full person removes anyone's.
	full := addPerson(t, deps, TierFull)
	if got := send(t, deps, "tcp", full, protocol.EventRemoveToken, `{"name":"theirs"}`); refused(got) {
		t.Errorf("Full couldn't remove a credential: %s", got)
	}
}

// Removing a credential disconnects its devices at once.
func TestRemoveTokenDisconnects(t *testing.T) {
	deps := newTestDeps(t)
	victim := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard))
	closed := make(chan struct{}, 1)
	victim.conn = closeFn(func() { closed <- struct{}{} })
	send(t, deps, "unix", nil, protocol.EventRemoveToken, `{"name":"`+victim.TokenName+`"}`)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the revoked device stayed connected")
	}
}

type closeFn func()

func (f closeFn) Close() error { f(); return nil }

// approve_pairing: a Trusted person answers only their own pairings, and can't hand out Full.
func TestApprovePairingByTier(t *testing.T) {
	deps := newTestDeps(t)
	deps.pairings = newPairingGate()
	tess := addPerson(t, deps, TierTrusted)
	other := addPerson(t, deps, TierTrusted)
	registerToken(t, deps, "tcp", tess, `{"name":"tess-code","pairing":true}`)
	registerToken(t, deps, "unix", nil, `{"name":"owner-code","pairing":true}`)

	if got := send(t, deps, "tcp", other, protocol.EventApprovePairing, `{"credential":"tess-code","allow":true}`); !refused(got) {
		t.Errorf("approved a pairing someone else made: %s", got)
	}
	if got := send(t, deps, "tcp", tess, protocol.EventApprovePairing, `{"credential":"owner-code","allow":true}`); !refused(got) {
		t.Errorf("approved the owner's pairing: %s", got)
	}
	if got := send(t, deps, "tcp", tess, protocol.EventApprovePairing, `{"credential":"tess-code","allow":true,"full":true}`); !refused(got) {
		t.Errorf("a Trusted person approved with full access: %s", got)
	}
	if deps.settings.TokenRestricted("tess-code") == false {
		t.Fatal("full access leaked through a refused approval")
	}
	if got := send(t, deps, "tcp", tess, protocol.EventApprovePairing, `{"credential":"tess-code","allow":true}`); refused(got) {
		t.Errorf("couldn't approve their own pairing: %s", got)
	}
	if got := send(t, deps, "unix", nil, protocol.EventApprovePairing, `{"credential":"owner-code","allow":true,"full":true}`); refused(got) {
		t.Errorf("the owner couldn't approve with full access: %s", got)
	}
	if deps.settings.TokenRestricted("owner-code") {
		t.Fatal("approving with full access didn't take")
	}
}

// pairing_request names whose device it will be and its tier, and reaches the Trusted person who made it.
func TestPairingRequestCarriesUserAndTier(t *testing.T) {
	deps := newTestDeps(t)
	deps.pairings = newPairingGate()
	var tessOut syncBuf
	tess := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &tessOut))
	registerToken(t, deps, "tcp", tess, `{"name":"code","pairing":true,"user":"new","newUserName":"Dee"}`)
	deps.settings.mu.Lock()
	rec := deps.settings.Tokens["code"]
	deps.settings.Tokens["code"] = rec
	deps.settings.mu.Unlock()
	pairingApprovalWait = 2 * time.Second
	t.Cleanup(func() { pairingApprovalWait = 60 * time.Second })
	done := make(chan error, 1)
	go func() {
		done <- deps.admitPairing(protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard), peerIdentity{TokenName: "code", DeviceID: "d1", Name: "Dee's phone"}, rec, "1.2.3.4")
	}()
	for i := 0; i < 200 && !deps.pairings.answerable("code"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	got := tessOut.String()
	if !strings.Contains(got, `"pairing_request"`) || !strings.Contains(got, `"userName":"Dee"`) || !strings.Contains(got, `"tier":"limited"`) {
		t.Errorf("the creator wasn't asked with the person and tier: %s", got)
	}
	send(t, deps, "tcp", tess, protocol.EventApprovePairing, `{"credential":"code","allow":true}`)
	if err := <-done; err != nil {
		t.Fatalf("pairing failed: %v", err)
	}
}

// The admin events change what they say, are audited-by-name, and are Full only.
func TestAdminEventsChangeAccess(t *testing.T) {
	deps := newTestDeps(t)
	full := addPerson(t, deps, TierFull)
	trusted := addPerson(t, deps, TierTrusted)
	phone := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard))

	for _, ev := range []struct {
		typ  protocol.EventType
		body string
	}{
		{protocol.EventSetUserPermissions, `{"userId":"` + phone.UserID + `","tier":"full"}`},
		{protocol.EventSetDeviceRestrict, `{"credential":"` + phone.TokenName + `","maxTier":""}`},
		{protocol.EventSetLimitedDefaults, `{"caps":{"mcp_tools":true}}`},
		{protocol.EventMergeUsers, `{"from":"` + phone.UserID + `","into":"owner"}`},
		{protocol.EventMoveDevice, `{"credential":"` + phone.TokenName + `","userId":"owner"}`},
		{protocol.EventRemoveUser, `{"userId":"` + phone.UserID + `"}`},
	} {
		if got := send(t, deps, "tcp", trusted, ev.typ, ev.body); !strings.Contains(got, "needs Full access") {
			t.Errorf("%s from Trusted: %s", ev.typ, got)
		}
		if got := send(t, deps, "tcp", phone, ev.typ, ev.body); !strings.Contains(got, "needs Full access") {
			t.Errorf("%s from Limited: %s", ev.typ, got)
		}
	}
	if deps.settings.Users[phone.UserID].Tier != TierLimited || len(deps.settings.LimitedDefaults) != 0 {
		t.Fatal("a refused admin event still changed something")
	}

	// set_user_permissions: tier and caps; the owner is untouchable; bad values are refused.
	if got := send(t, deps, "tcp", full, protocol.EventSetUserPermissions, `{"userId":"owner","tier":"limited"}`); !refused(got) {
		t.Error("the owner's tier was changed")
	}
	if got := send(t, deps, "tcp", full, protocol.EventSetUserPermissions, `{"userId":"`+phone.UserID+`","caps":{"nope":true}}`); !refused(got) {
		t.Error("an unknown capability was accepted")
	}
	if got := send(t, deps, "tcp", full, protocol.EventSetUserPermissions, `{"userId":"`+phone.UserID+`","tier":"trusted","caps":{"mcp_tools":true,"browser":"watch"}}`); refused(got) {
		t.Errorf("set_user_permissions: %s", got)
	}
	if a := deps.settings.Access("tcp", phone.TokenName); a.Tier != TierTrusted || !a.Caps.MCPTools || a.Caps.Browser != browserWatch {
		t.Errorf("access after the change = %+v", a)
	}
	// ...and that applied at once to the live connection.
	if p, _ := deps.peers.Get(phone.ID); p.Tier != TierTrusted {
		t.Errorf("the live connection still says %q", p.Tier)
	}

	// A device ceiling and denials.
	if got := send(t, deps, "tcp", full, protocol.EventSetDeviceRestrict, `{"credential":"`+phone.TokenName+`","maxTier":"limited","deny":["mcp_tools"]}`); refused(got) {
		t.Errorf("set_device_restrictions: %s", got)
	}
	if a := deps.settings.Access("tcp", phone.TokenName); a.Tier != TierLimited || a.Caps.MCPTools {
		t.Errorf("access after the device restriction = %+v", a)
	}
	if got := send(t, deps, "tcp", full, protocol.EventSetDeviceRestrict, `{"credential":"`+phone.TokenName+`","deny":["bogus"]}`); !refused(got) {
		t.Error("an unknown denial was accepted")
	}

	// Defaults for Limited people.
	if got := send(t, deps, "tcp", full, protocol.EventSetLimitedDefaults, `{"caps":{"terminals_watch":true}}`); refused(got) {
		t.Errorf("set_limited_defaults: %s", got)
	}
	if a := deps.settings.Access("tcp", phone.TokenName); !a.Caps.TerminalsWatch {
		t.Error("the default for Limited people didn't apply")
	}

	// Merge: the devices move, the names are kept, and the old id still resolves.
	extra, _ := deps.settings.CreateUser("Old Name", "", TierLimited, ownerID)
	deps.settings.AddCredential(NewCredential{Name: "old-dev", Token: "tok-old", UserID: extra.ID})
	if got := send(t, deps, "tcp", full, protocol.EventMergeUsers, `{"from":"owner","into":"`+extra.ID+`"}`); !refused(got) {
		t.Error("the owner was merged away")
	}
	if got := send(t, deps, "tcp", full, protocol.EventMergeUsers, `{"from":"`+extra.ID+`","into":"`+extra.ID+`"}`); !refused(got) {
		t.Error("merged a person into themselves")
	}
	if got := send(t, deps, "tcp", full, protocol.EventMergeUsers, `{"from":"`+extra.ID+`","into":"`+phone.UserID+`"}`); refused(got) {
		t.Fatalf("merge_users: %s", got)
	}
	into, _ := deps.settings.GetUser(phone.UserID)
	if !contains(into.Names, "Old Name") {
		t.Errorf("names not copied: %+v", into.Names)
	}
	if rec, _ := deps.settings.TokenRecordOf("old-dev"); rec.UserID != phone.UserID {
		t.Errorf("device not moved: %+v", rec)
	}
	if a, _ := deps.settings.GetUser(extra.ID); a.AliasOf != phone.UserID {
		t.Errorf("alias not set: %+v", a)
	}
	if people := send(t, deps, "tcp", full, protocol.EventListPeople, `{}`); strings.Contains(people, extra.ID) {
		t.Errorf("a merged person still listed: %s", people)
	}

	// Move a device.
	if got := send(t, deps, "tcp", full, protocol.EventMoveDevice, `{"credential":"old-dev","userId":"owner"}`); refused(got) {
		t.Errorf("move_device: %s", got)
	}
	if rec, _ := deps.settings.TokenRecordOf("old-dev"); rec.UserID != ownerID {
		t.Errorf("device not moved to the owner: %+v", rec)
	}
	if got := send(t, deps, "tcp", full, protocol.EventMoveDevice, `{"credential":"old-dev","userId":"u-nobody"}`); !refused(got) {
		t.Error("moved a device to nobody")
	}

	// Remove a person: their devices are revoked and disconnected; the owner can't be removed.
	closed := make(chan struct{}, 1)
	phone.conn = closeFn(func() { closed <- struct{}{} })
	if got := send(t, deps, "tcp", full, protocol.EventRemoveUser, `{"userId":"owner"}`); !refused(got) {
		t.Error("the owner was removed")
	}
	if got := send(t, deps, "tcp", full, protocol.EventRemoveUser, `{"userId":"`+phone.UserID+`"}`); refused(got) {
		t.Fatalf("remove_user: %s", got)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the removed person stayed connected")
	}
	if _, ok := deps.settings.TokenRecordOf(phone.TokenName); ok {
		t.Error("the removed person's credential survived")
	}
	if _, ok := deps.settings.GetUser(extra.ID); ok {
		t.Error("someone merged into the removed person survived")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Everyone who may read people is sent the new list after a change, and Limited people aren't.
func TestPeopleBroadcastReachesOnlyThoseWhoMayRead(t *testing.T) {
	deps := newTestDeps(t)
	var fullOut, limOut syncBuf
	full := addPersonOn(t, deps, TierFull, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &fullOut))
	addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &limOut))
	send(t, deps, "tcp", full, protocol.EventSetLimitedDefaults, `{"caps":{"mcp_tools":true}}`)
	if !strings.Contains(fullOut.String(), `"type":"people"`) {
		t.Error("the Full device wasn't sent people")
	}
	if strings.Contains(limOut.String(), `"type":"people"`) {
		t.Error("a Limited device was sent people")
	}
}

// server_identity and peer_list carry the tier, the capabilities, and the person.
func TestIdentityAndRosterCarryTierAndPerson(t *testing.T) {
	deps := newTestDeps(t)
	deps.peers.UserName = deps.settings.UserName
	var out syncBuf
	p := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &out))
	u := deps.settings.Users[p.UserID]
	u.Name = "Tess"
	deps.settings.Users[p.UserID] = u
	deps.peers.Broadcast()
	if got := out.String(); !strings.Contains(got, `"tier":"trusted"`) || !strings.Contains(got, `"userId":"`+p.UserID+`"`) || !strings.Contains(got, `"userName":"Tess"`) {
		t.Errorf("peer_list: %s", got)
	}
	// The local socket is the owner, full.
	var lo syncBuf
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &lo), nopCloser{}, "unix", peerIdentity{Name: "app"})
	deps.peers.Broadcast()
	if got := lo.String(); !strings.Contains(got, `"tier":"full"`) || !strings.Contains(got, `"userId":"owner"`) {
		t.Errorf("peer_list for the local peer: %s", got)
	}
}
