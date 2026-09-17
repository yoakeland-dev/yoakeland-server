package provider

import "testing"

func TestClaudeArgs(t *testing.T) {
	args := claudeArgs("claude-3-5-sonnet-20241022", "hello world")
	want := []string{"-p", "--output-format", "json", "--disallowedTools", disallowedClaudeTools, "--model", "claude-3-5-sonnet-20241022", "--", "hello world"}
	if !equalStrings(args, want) {
		t.Fatalf("claudeArgs = %v, want %v", args, want)
	}

	// Without a model, no --model flag should appear.
	args = claudeArgs("", "hi")
	for _, a := range args {
		if a == "--model" {
			t.Fatalf("claudeArgs with empty model should omit --model, got %v", args)
		}
	}
}

func TestCodexArgs(t *testing.T) {
	args := codexArgs("gpt-5", "do the thing")
	want := []string{"exec", "--json", "--sandbox", "read-only", "-c", "tools.web_search=true", "--model", "gpt-5", "--", "do the thing"}
	if !equalStrings(args, want) {
		t.Fatalf("codexArgs = %v, want %v", args, want)
	}
}

func TestParseClaudeOutput_Success(t *testing.T) {
	raw := []byte(`{"result":"hello from claude","is_error":false,"total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":50}}`)
	text, chunk, err := parseClaudeOutput(raw)
	if err != nil {
		t.Fatalf("parseClaudeOutput: %v", err)
	}
	if text != "hello from claude" {
		t.Fatalf("text = %q", text)
	}
	if chunk.Usage == nil || chunk.Usage.InputTokens != 100 || chunk.Usage.OutputTokens != 50 || chunk.Usage.CostUSD != 0.0123 {
		t.Fatalf("unexpected usage: %+v", chunk.Usage)
	}
}

func TestParseClaudeOutput_ErrorResult(t *testing.T) {
	raw := []byte(`{"result":"boom","is_error":true}`)
	if _, _, err := parseClaudeOutput(raw); err == nil {
		t.Fatal("expected an error when is_error is true")
	}
}

func TestParseClaudeOutput_Malformed(t *testing.T) {
	if _, _, err := parseClaudeOutput([]byte("not json at all")); err == nil {
		t.Fatal("expected an error on malformed claude CLI output")
	}
}

func TestParseCodexOutput_Success(t *testing.T) {
	raw := []byte("" +
		`{"type":"thread.started"}` + "\n" +
		`{"type":"item.completed","item":{"type":"reasoning","text":"thinking..."}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"hello from codex"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":30,"output_tokens":10}}` + "\n")

	text, chunk, err := parseCodexOutput(raw)
	if err != nil {
		t.Fatalf("parseCodexOutput: %v", err)
	}
	if text != "hello from codex" {
		t.Fatalf("text = %q, want the agent_message text (not the reasoning item)", text)
	}
	if chunk.Usage == nil || chunk.Usage.InputTokens != 30 || chunk.Usage.OutputTokens != 10 {
		t.Fatalf("unexpected usage: %+v", chunk.Usage)
	}
}

func TestParseCodexOutput_TurnFailed(t *testing.T) {
	raw := []byte(`{"type":"turn.failed"}` + "\n")
	if _, _, err := parseCodexOutput(raw); err == nil {
		t.Fatal("expected an error on turn.failed")
	}
}

func TestParseCodexOutput_NoAgentMessage(t *testing.T) {
	raw := []byte(`{"type":"thread.started"}` + "\n")
	if _, _, err := parseCodexOutput(raw); err == nil {
		t.Fatal("expected an error when no agent_message item is present")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
