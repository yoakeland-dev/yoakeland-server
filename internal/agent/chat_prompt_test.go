package agent

import "testing"

func TestActiveSystemPromptChat(t *testing.T) {
	a := &Agent{chat: true}
	if a.activeSystemPrompt() != ChatSystemPrompt {
		t.Fatal("a chat should get ChatSystemPrompt")
	}
	a.mode = ModePlan
	if a.activeSystemPrompt() != PlanSystemPrompt {
		t.Fatal("plan mode still plans")
	}
	if (&Agent{}).activeSystemPrompt() != DefaultSystemPrompt {
		t.Fatal("a project session keeps the default prompt")
	}
}
