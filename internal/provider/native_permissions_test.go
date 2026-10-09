package provider

import (
	"encoding/json"
	"testing"
)

func TestClaudeAskTarget(t *testing.T) {
	for _, c := range []struct{ tool, in, wantTool, wantTarget string }{
		{"Bash", `{"command":"git push origin main"}`, "bash", "git push origin main"},
		{"Write", `{"file_path":"/p/a.go"}`, "file_write", "/p/a.go"},
		{"Edit", `{"file_path":"/p/a.go"}`, "file_edit", "/p/a.go"},
		{"MultiEdit", `{"file_path":"/p/b.go"}`, "file_edit", "/p/b.go"},
		{"WebFetch", `{"url":"https://example.com/x?y=1"}`, "web_fetch", "example.com"},
		{"Read", `{"file_path":"/p/a.go"}`, "", ""},
	} {
		tool, target := claudeAskTarget(c.tool, json.RawMessage(c.in))
		if tool != c.wantTool || target != c.wantTarget {
			t.Errorf("%s = %q %q, want %q %q", c.tool, tool, target, c.wantTool, c.wantTarget)
		}
	}
}

func TestClaudeSuggestionsLabelledAndPicked(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test:*"}],"behavior":"allow","destination":"localSettings"}`),
		json.RawMessage(`{"type":"setMode","mode":"acceptEdits","destination":"session"}`),
		json.RawMessage(`{"type":"addDirectories","directories":["/tmp/x"],"destination":"session"}`),
	}
	got := claudeSuggestions(raw)
	want := []string{
		"Always allow Bash(npm test:*) in this project (local) (Claude's rule)",
		"Switch Claude to acceptEdits for this session",
		"Allow access to /tmp/x for this session",
	}
	for i, w := range want {
		if got[i].Label != w || got[i].ID != "native:"+string(rune('0'+i)) {
			t.Errorf("suggestion %d = %+v, want label %q", i, got[i], w)
		}
	}
	if p := claudeChosenSuggestions(raw, "native:1"); len(p) != 1 || string(p[0]) != string(raw[1]) {
		t.Errorf("pick = %s", p)
	}
	if p := claudeChosenSuggestions(raw, ""); len(p) != 3 {
		t.Errorf("no pick should keep all, got %d", len(p))
	}
	if p := claudeChosenSuggestions(raw, "native:9"); len(p) != 0 {
		t.Errorf("out-of-range pick should grant nothing, got %d", len(p))
	}
}
