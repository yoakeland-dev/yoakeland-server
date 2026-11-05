package main

import (
	"testing"

	"yoakeland/internal/protocol"
)

// "default" is where handlers land when no chat is named. Until the owner makes it, it is theirs:
// nobody below Full can claim it by asking first.
func TestDefaultChatCantBeSquatted(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	trusted := addPerson(t, deps, TierTrusted)
	for _, p := range []*Peer{a, trusted} {
		for _, env := range []protocol.Envelope{
			envOf(protocol.EventUserPrompt, "default", protocol.UserPromptData{Prompt: "x", SessionID: "default", ProjectPath: root}),
			envOf(protocol.EventUserPrompt, "", protocol.UserPromptData{Prompt: "x", ProjectPath: root}),
			envOf(protocol.EventSetMode, "", protocol.SetModeData{Mode: "read_only", ProjectPath: root}),
			envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: "default", ProjectPath: root}),
		} {
			if out := sendAs(t, deps, p, env); !notTheirChat(out) {
				t.Errorf("%s on a default chat nobody made: %q", env.Type, out)
			}
		}
	}
	if _, ok := deps.projects.cachedOwner("default"); ok {
		t.Error("default was claimed")
	}
	if out := sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "default", protocol.UserPromptData{Prompt: "x", SessionID: "default", ProjectPath: root})); refused(out) {
		t.Errorf("the owner can't start it: %s", out)
	}
	if got := ownerOf(t, deps, "default"); got != ownerID {
		t.Errorf("default is owned by %q", got)
	}
	if out := sendAs(t, deps, a, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: "default", ProjectPath: root})); !notTheirChat(out) {
		t.Errorf("a Limited person reads default: %s", out)
	}
}
