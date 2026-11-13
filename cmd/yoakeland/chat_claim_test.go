package main

import (
	"fmt"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// After a restart nothing is cached or bound. Naming someone else's chat together with another
// project (or the chat project) finds no chat there, and must not make the sender its owner.
func TestChatInAnotherProjectCantBeClaimed(t *testing.T) {
	for _, wrong := range []string{"other", protocol.ChatProject} {
		deps, _, root := policyDeps(t)
		path := wrong
		if wrong == "other" {
			path = t.TempDir()
			if _, err := deps.projects.Get(path); err != nil {
				t.Fatal(err)
			}
		}
		a := addPerson(t, deps, TierLimited)
		sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "the secret", SessionID: "s-o", ProjectPath: root}))
		deps.projects.mu.Lock()
		deps.projects.owners = nil
		deps.projects.sessionProject = map[string]string{}
		deps.projects.mu.Unlock()

		// cancel_turn's handler ignores projectPath, but the ownership check reads it.
		if out := sendAs(t, deps, a, envOf(protocol.EventCancelTurn, "s-o", map[string]any{"turnId": "x", "projectPath": path})); !notTheirChat(out) {
			t.Errorf("%s: the claim wasn't refused: %q", wrong, out)
		}
		out := sendAs(t, deps, a, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: "s-o", ProjectPath: root}))
		if strings.Contains(out, "the secret") || !notTheirChat(out) {
			t.Errorf("%s: a stranger read the owner's chat: %.200s", wrong, out)
		}
		if out := sendAs(t, deps, nil, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: "s-o", ProjectPath: root})); !strings.Contains(out, "the secret") {
			t.Errorf("%s: the owner lost their chat: %.200s", wrong, out)
		}
	}
}

// A page answers once, and a client inventing artifact ids runs out of them.
func TestArtifactSubmitsAreBounded(t *testing.T) {
	const chat = "s-artifact-bound"
	defer artifactForget(chat)
	if !artifactSubmitAllow(chat, "a0") || artifactSubmitAllow(chat, "a0") {
		t.Fatal("an artifact submits exactly once")
	}
	for i := 1; i < maxSubmittedArtifactsPerChat; i++ {
		if !artifactSubmitAllow(chat, fmt.Sprint("a", i)) {
			t.Fatalf("artifact %d was refused", i)
		}
	}
	if artifactSubmitAllow(chat, "one-too-many") {
		t.Error("a chat submitted from more artifacts than the cap")
	}
	artifactForget(chat)
	if !artifactSubmitAllow(chat, "a0") {
		t.Error("a deleted chat's count wasn't dropped")
	}
}
