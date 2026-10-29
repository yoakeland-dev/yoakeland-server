package provider

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
)

// claudeStub answers each launch with the next reply, recording the --model it was given.
func claudeStub(models *[]string, sent *strings.Builder, replies ...string) *CLISubprocessProvider {
	return &CLISubprocessProvider{Kind: CLIKindClaude, Model: "opus", Start: func(_ context.Context, _ string, args []string, _ string) (*nativeProc, error) {
		for i, a := range args {
			if a == "--model" {
				*models = append(*models, args[i+1])
			}
		}
		reply := replies[len(*models)-1]
		return &nativeProc{stdin: nopWriteCloser{sent}, lines: bufio.NewScanner(strings.NewReader(reply + "\n")), wait: func() error {
			if strings.Contains(reply, `"is_error":true`) {
				return errors.New("exit status 1")
			}
			return nil
		}}, nil
	}}
}

func TestClaudeOneShotRunsToollessAndReturnsTheAnswer(t *testing.T) {
	var models []string
	var sent strings.Builder
	p := claudeStub(&models, &sent, `{"result":"A Title","is_error":false,"total_cost_usd":0.001,"usage":{"input_tokens":2,"output_tokens":5,"cache_read_input_tokens":10}}`)
	text, usage, err := p.OneShot(context.Background(), "sys", "fix the login bug")
	if err != nil || text != "A Title" || sent.String() != "fix the login bug" || models[0] != "haiku" {
		t.Fatalf("got %q %v, sent %q, models %v", text, err, sent.String(), models)
	}
	if usage.CostUSD != 0.001 || usage.InputTokens != 12 || usage.OutputTokens != 5 || usage.CachedInputTokens != 10 {
		t.Fatalf("usage %+v", usage)
	}
	if _, _, err := (&CLISubprocessProvider{Kind: CLIKindOpenCode}).OneShot(context.Background(), "s", "p"); err == nil {
		t.Fatal("an ACP agent has no one-shot mode")
	}
}

func TestClaudeOneShotReportsTheRealErrorAndRetriesOnTheChatModel(t *testing.T) {
	var models []string
	var sent strings.Builder
	refused := `{"is_error":true,"result":"There's an issue with the selected model (haiku)."}`
	p := claudeStub(&models, &sent, refused, `{"result":"Fine","is_error":false}`)
	if text, _, err := p.OneShot(context.Background(), "s", "p"); err != nil || text != "Fine" || strings.Join(models, ",") != "haiku,opus" {
		t.Fatalf("got %q %v after %v", text, err, models)
	}

	models = nil
	p = claudeStub(&models, &sent, refused, refused)
	_, _, err := p.OneShot(context.Background(), "s", "p")
	if err == nil || !strings.Contains(err.Error(), "issue with the selected model") {
		t.Fatalf("the reason on stdout was lost: %v", err)
	}
}
