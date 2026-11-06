package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// Below Full a snapshot is restored only through the chat it was taken in, by someone who owns that
// chat and may edit files; a snapshot with no recorded chat can't be tied to anyone.
func TestSnapshotsFollowTheirChat(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "hi", SessionID: "s-o", ProjectPath: root}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "hi", SessionID: "s-a", ProjectPath: root}))
	pc, _ := deps.projects.Get(root)
	snapOf := func(session string) string {
		file := filepath.Join(root, "f-"+session+".txt") // one file each: a restore refuses a file others changed since
		_ = os.WriteFile(file, []byte("v1"), 0o644)
		s, _, err := pc.SnapStore.CapturePreMutation(session, "t-"+session, "turn", root, file)
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	mine, theirs, old := snapOf("s-a"), snapOf("s-o"), snapOf("")

	restore := func(p *Peer, session, id string) string {
		return sendAs(t, deps, p, envOf(protocol.EventRestoreSnapshot, session, protocol.RestoreSnapshotData{SnapshotID: id, ProjectPath: root}))
	}
	if out := restore(a, "s-a", mine); refused(out) {
		t.Errorf("their own chat's snapshot was refused: %s", out)
	}
	for name, out := range map[string]string{
		"another chat's snapshot through their own chat": restore(a, "s-a", theirs),
		"their snapshot through another person's chat":   restore(a, "s-o", theirs),
		"no chat at all":                   restore(a, "", mine),
		"a snapshot with no recorded chat": restore(a, "s-a", old),
	} {
		if !refused(out) {
			t.Errorf("%s was allowed: %s", name, out)
		}
	}
	// Without files_edit nothing is restored, even from their own chat.
	setUser(t, deps, a.UserID, func(u *User) { u.Caps = map[string]any{CapFilesEdit: false} })
	if out := restore(a, "s-a", mine); !strings.Contains(out, "isn't allowed for this person") {
		t.Errorf("restore without files_edit: %s", out)
	}
	// Full restores any of them.
	full := addPerson(t, deps, TierFull)
	if out := restore(full, "", old); refused(out) {
		t.Errorf("Full refused: %s", out)
	}

	list := func(p *Peer, session string) string {
		return sendAs(t, deps, p, envOf(protocol.EventListSnapshots, session, protocol.ListSnapshotsData{ProjectPath: root}))
	}
	if out := list(a, ""); !refused(out) {
		t.Errorf("a sessionless list was allowed: %s", out)
	}
	out := list(a, "s-a")
	if !strings.Contains(out, mine) || strings.Contains(out, theirs) || strings.Contains(out, old) {
		t.Errorf("list for their chat = %s", out)
	}
	if out := list(a, "s-o"); !notTheirChat(out) {
		t.Errorf("another person's chat listed: %s", out)
	}
	if out := list(nil, ""); !strings.Contains(out, mine) || !strings.Contains(out, theirs) || !strings.Contains(out, old) {
		t.Errorf("the owner's list = %s", out)
	}
}
