package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
)

// history reads a chat's session_history the way a client does.
func history(t *testing.T, deps connDeps, from *Peer, session string) protocol.SessionHistoryData {
	t.Helper()
	out := sendAs(t, deps, from, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: session, ProjectPath: deps.defaultProjectRoot}))
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &env); err != nil || env.Type != protocol.EventSessionHistory {
		t.Fatalf("no session_history: %q", out)
	}
	var d protocol.SessionHistoryData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func setUser(t *testing.T, deps connDeps, id string, edit func(*User)) {
	t.Helper()
	deps.settings.mu.Lock()
	defer deps.settings.mu.Unlock()
	u := deps.settings.Users[id]
	edit(&u)
	deps.settings.Users[id] = u
}

func sessionFile(t *testing.T, deps connDeps, id string) string {
	return filepath.Join(projectstore.DataDir(deps.projects.stateDir, deps.defaultProjectRoot), "sessions", id+".json")
}

// Messages from before authorId get one by matching the author name against current and earlier
// names, following merges, without rewriting the file; the rest keep what they were stored with.
func TestOldMessagesAreMatchedToPeopleWithoutRewritingTheFile(t *testing.T) {
	deps, _, _ := policyDeps(t)
	ada := addPerson(t, deps, TierLimited)
	bob := addPerson(t, deps, TierLimited)
	cy := addPerson(t, deps, TierLimited)
	dee := addPerson(t, deps, TierLimited)
	setUser(t, deps, ada.UserID, func(u *User) { u.Name, u.Names, u.Avatar = "Ada Lovelace", []string{"Ada"}, "🧮" })
	setUser(t, deps, bob.UserID, func(u *User) { u.Name, u.Avatar = "Bob", "🔧" })
	setUser(t, deps, cy.UserID, func(u *User) { u.Name, u.Names = "Cy", []string{"Sam"} })
	setUser(t, deps, dee.UserID, func(u *User) { u.Name, u.Names = "Dee", []string{"Sam"} }) // two people were once "Sam"
	// Bob is merged away: his old messages carry his old id, and his names follow him.
	eve := addPerson(t, deps, TierLimited)
	setUser(t, deps, eve.UserID, func(u *User) { u.Name, u.Avatar = "Eve", "🌿" })
	setUser(t, deps, bob.UserID, func(u *User) { u.Names = []string{"Robert"} })
	if err := deps.settings.MergeUsers(bob.UserID, eve.UserID); err != nil {
		t.Fatal(err)
	}

	msg := func(id, author, authorID, avatar string) agent.Message {
		return agent.Message{ID: id, Role: "user", Content: id, AuthorName: author, AuthorID: authorID, AuthorAvatar: avatar}
	}
	err := projectStore(t, deps).Save(&agent.SessionState{SessionID: "old", OwnerID: ownerID, History: []agent.Message{
		msg("m1", "Ada", "", "🦄"),            // an earlier name
		msg("m2", "Ada Lovelace", "", ""),    // the current name
		msg("m3", "Robert", "", ""),          // a merged person's earlier name
		msg("m4", "Sam", "", "🐙"),            // ambiguous
		msg("m5", "Zed", "", "🦓"),            // nobody
		msg("m6", "Bob", bob.UserID, ""),     // stored under an id that has since been merged
		msg("m7", "Gone", "u-removed", "🕳️"), // a person who was removed
		{ID: "m8", Role: "assistant", Content: "hi"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(sessionFile(t, deps, "old"))

	full := addPerson(t, deps, TierFull)
	d := history(t, deps, full, "old")
	by := map[string]protocol.HistoryMessage{}
	for _, m := range d.Messages {
		by[m.ID] = m
	}
	for id, want := range map[string]string{"m1": ada.UserID, "m2": ada.UserID, "m3": eve.UserID, "m4": "", "m5": "", "m6": eve.UserID, "m7": "u-removed", "m8": ""} {
		if got := by[id].AuthorID; got != want {
			t.Errorf("%s: authorId = %q, want %q", id, got, want)
		}
	}
	if by["m1"].Author != "Ada" || by["m1"].AuthorAvatar != "🦄" {
		t.Errorf("a matched old message lost what it was stored with: %+v", by["m1"])
	}
	if by["m4"].Author != "Sam" || by["m4"].AuthorAvatar != "🐙" || by["m5"].AuthorAvatar != "🦓" {
		t.Errorf("an unplaced message lost its stored name or picture: %+v %+v", by["m4"], by["m5"])
	}
	if p := d.Profiles[ada.UserID]; p.Name != "Ada Lovelace" || p.Avatar != "🧮" {
		t.Errorf("Ada's profile = %+v", p)
	}
	if p := d.Profiles[eve.UserID]; p.Name != "Eve" || p.Avatar != "🌿" {
		t.Errorf("a merged person's profile = %+v", p)
	}
	if p := d.Profiles["u-removed"]; p.Name != "Gone" || p.Avatar != "🕳️" {
		t.Errorf("a removed person keeps their stored name and picture: %+v", p)
	}
	if _, ok := d.Profiles[bob.UserID]; ok || len(d.Profiles) != 3 {
		t.Errorf("profiles = %+v, want each author once under the person they are now", d.Profiles)
	}
	if after, _ := os.ReadFile(sessionFile(t, deps, "old")); !bytes.Equal(before, after) {
		t.Error("reading the history rewrote the session file")
	}
}

// New messages keep the author's id and name but no picture; the live event carries the current
// one; and a message stashed on a branch (an edit, then switching back) is still attributed.
func TestNewMessagesStoreTheAuthorsIDNotTheirPicture(t *testing.T) {
	deps, _, root := policyDeps(t)
	deps = wireVisibility(deps)
	var watcherOut syncBuf
	a := addPerson(t, deps, TierLimited)
	setUser(t, deps, a.UserID, func(u *User) { u.Name, u.Avatar = "Ann", "🦊" })
	watcher := addPersonOn(t, deps, TierFull, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &watcherOut))
	pc, _ := deps.projects.Get(root)
	pc.Registry.Get(watcher.ipc, "s1")

	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s1", protocol.UserPromptData{Prompt: "one", SessionID: "s1", ProjectPath: root, AuthorName: "x", AuthorAvatar: "🦖"}))
	if !strings.Contains(watcherOut.String(), `"authorId":"`+a.UserID+`"`) || !strings.Contains(watcherOut.String(), `"author":"Ann"`) || !strings.Contains(watcherOut.String(), `"authorAvatar":"🦊"`) {
		t.Errorf("user_message = %s", watcherOut.String())
	}
	raw, _ := os.ReadFile(sessionFile(t, deps, "s1"))
	if bytes.Contains(raw, []byte("authorAvatar")) || !bytes.Contains(raw, []byte(`"authorId": "`+a.UserID+`"`)) || !bytes.Contains(raw, []byte(`"author": "Ann"`)) {
		t.Errorf("stored session: %s", raw)
	}

	// Edit the first message, then switch back to the stashed original.
	sendAs(t, deps, a, envOf(protocol.EventEditMessage, "s1", protocol.EditMessageData{SessionID: "s1", MessageID: "m1", Content: "two"}))
	d := history(t, deps, a, "s1")
	if len(d.Messages) < 1 || len(d.Messages[0].AlternateIDs) != 1 {
		t.Fatalf("no alternate after the edit: %+v", d.Messages)
	}
	out := sendAs(t, deps, a, envOf(protocol.EventSwitchBranch, "s1", protocol.SwitchBranchData{SessionID: "s1", MessageID: d.Messages[0].AlternateIDs[0]}))
	var env protocol.Envelope
	_ = json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &env)
	var back protocol.SessionHistoryData
	_ = json.Unmarshal(env.Data, &back)
	if len(back.Messages) == 0 || back.Messages[0].Content != "one" || back.Messages[0].AuthorID != a.UserID || back.Profiles[a.UserID].Avatar != "🦊" {
		t.Errorf("after switching back: %s", out)
	}
	// The other branch, stashed by that switch, carries the editor's id too.
	if st, _ := projectStore(t, deps).Load("s1"); len(st.Branches) == 0 {
		t.Error("no stashed branch")
	} else {
		for _, alts := range st.Branches {
			for _, alt := range alts {
				if alt[0].AuthorID != a.UserID {
					t.Errorf("a stashed copy lost its author: %+v", alt[0])
				}
			}
		}
	}
}

// set_profile edits your own person, adds the old name to Names, tells every peer and the person's
// own devices, and refuses an oversized picture or an empty name.
func TestSetProfile(t *testing.T) {
	deps, _, _ := policyDeps(t)
	var aOut, aPhoneOut, otherOut syncBuf
	a := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &aOut))
	aPhone := addDeviceOf(t, deps, a.UserID, "", &aPhoneOut)
	other := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &otherOut))
	setUser(t, deps, a.UserID, func(u *User) { u.Name = "Ann" })
	otherBefore, _ := deps.settings.GetUser(other.UserID)

	if out := send(t, deps, "tcp", a, protocol.EventSetProfile, `{"name":"Anna","avatar":"🦊"}`); refused(out) {
		t.Fatalf("set_profile refused: %s", out)
	}
	u, _ := deps.settings.GetUser(a.UserID)
	if u.Name != "Anna" || u.Avatar != "🦊" || len(u.Names) != 1 || u.Names[0] != "Ann" {
		t.Fatalf("user = %+v", u)
	}
	want := `"type":"profile_updated","data":{"id":"` + a.UserID + `","name":"Anna","avatar":"🦊"}`
	for name, out := range map[string]*syncBuf{"the sender": &aOut, "their other device": &aPhoneOut, "another person": &otherOut} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%s never got profile_updated: %s", name, out.String())
		}
	}
	if !strings.Contains(aPhoneOut.String(), `"type":"server_identity"`) || !strings.Contains(aPhoneOut.String(), `"user":{"id":"`+a.UserID+`","name":"Anna"`) {
		t.Errorf("their device wasn't told who it is: %s", aPhoneOut.String())
	}
	// Taking the old name back removes it from Names; omitted fields stay as they are.
	send(t, deps, "tcp", aPhone, protocol.EventSetProfile, `{"name":"Ann"}`)
	if u, _ := deps.settings.GetUser(a.UserID); u.Name != "Ann" || u.Avatar != "🦊" || len(u.Names) != 1 || u.Names[0] != "Anna" {
		t.Errorf("after taking a name back: %+v", u)
	}
	// Refusals.
	for _, bad := range []string{`{"name":"  "}`, `{"avatar":"` + strings.Repeat("x", maxAvatarLen+1) + `"}`} {
		if out := send(t, deps, "tcp", a, protocol.EventSetProfile, bad); !refused(out) {
			t.Errorf("accepted %.40s", bad)
		}
	}
	// It never reaches anyone else's person.
	if got, _ := deps.settings.GetUser(other.UserID); got.Name != otherBefore.Name || got.Avatar != otherBefore.Avatar {
		t.Errorf("another person changed: %+v", got)
	}
	// The people list carries the names.
	if p := deps.peopleFor(localAccess()); func() bool {
		for _, x := range p.Users {
			if x.ID == a.UserID {
				return len(x.Names) == 1 && x.Names[0] == "Anna" && x.Name == "Ann"
			}
		}
		return false
	}() == false {
		t.Errorf("people = %+v", p.Users)
	}
}

// server_identity says who the connection is, and a merge keeps working for set_profile.
func TestServerIdentityCarriesTheUser(t *testing.T) {
	deps := newTestDeps(t)
	var out syncBuf
	a := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &out))
	setUser(t, deps, a.UserID, func(u *User) { u.Name, u.Avatar = "Ann", "🦊" })
	sendServerIdentity(deps, a)
	if !strings.Contains(out.String(), `"user":{"id":"`+a.UserID+`","name":"Ann","avatar":"🦊"}`) || !strings.Contains(out.String(), `"restricted":true`) {
		t.Errorf("server_identity = %s", out.String())
	}
	out.Reset()
	l := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &out), nopCloser{}, "unix", peerIdentity{Name: "pc"})
	sendServerIdentity(deps, l)
	if !strings.Contains(out.String(), `"user":{"id":"owner"`) {
		t.Errorf("the local socket's server_identity = %s", out.String())
	}
}
