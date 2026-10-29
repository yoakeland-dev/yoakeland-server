package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"yoakeland/internal/protocol"
)

const oneShotTimeout = 90 * time.Second

// OneShot implements agent.OneShotter: one tool-less request in a throwaway process, apart from
// the chat's own agent. Claude Code runs its small model and Codex its default at low effort; the
// ACP agents have no such mode. Run from the temp directory so a project's CLAUDE.md and the like
// don't ride along, with no tools, no skills, no hooks and no saved session.
func (p *CLISubprocessProvider) OneShot(ctx context.Context, system, prompt string) (string, protocol.UsageData, error) {
	ctx, cancel := context.WithTimeout(ctx, oneShotTimeout)
	defer cancel()
	switch p.Kind {
	case CLIKindClaude:
		// haiku is cheap, but a gateway or an account without it refuses the alias; the chat's own model then.
		text, usage, err := p.claudeOneShot(ctx, "haiku", system, prompt)
		if err != nil && p.Model != "" && p.Model != "haiku" && ctx.Err() == nil {
			text, usage, err = p.claudeOneShot(ctx, p.Model, system, prompt)
		}
		return text, usage, err
	case CLIKindCodex:
		text, err := p.codexOneShot(ctx, system, prompt)
		return text, protocol.UsageData{}, err
	}
	return "", protocol.UsageData{}, fmt.Errorf("%s can't answer a one-off request; assign a utility model in Settings", p.Kind)
}

// claudeResult is the one JSON object `claude -p --output-format json` prints, errors included:
// a failure's reason is in result on stdout, not on stderr.
type claudeResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheReadTokens     int `json:"cache_read_input_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (p *CLISubprocessProvider) claudeOneShot(ctx context.Context, model, system, prompt string) (string, protocol.UsageData, error) {
	var usage protocol.UsageData
	args := []string{"-p", "--model", model, "--output-format", "json", "--no-session-persistence", "--tools", "",
		"--disable-slash-commands", "--setting-sources", "", "--settings", `{"disableAllHooks":true}`,
		"--strict-mcp-config", "--system-prompt", system}
	proc, err := p.start()(ctx, "claude", args, os.TempDir())
	if err != nil {
		return "", usage, err
	}
	_, _ = fmt.Fprint(proc.stdin, prompt)
	_ = proc.stdin.Close()
	var out []string
	for proc.lines.Scan() {
		out = append(out, proc.lines.Text())
	}
	waitErr := proc.wait()
	var res claudeResult
	parsed := json.Unmarshal([]byte(strings.Join(out, "\n")), &res) == nil
	if parsed {
		u := res.Usage
		usage = protocol.UsageData{
			InputTokens:           u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens,
			OutputTokens:          u.OutputTokens,
			CachedInputTokens:     u.CacheReadTokens,
			CacheWriteInputTokens: u.CacheCreationTokens,
			CostUSD:               res.TotalCostUSD,
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	text := strings.TrimSpace(res.Result)
	switch {
	case parsed && res.IsError:
		return "", usage, fmt.Errorf("claude: %s", text)
	case waitErr != nil:
		return "", usage, fmt.Errorf("claude: %w%s", waitErr, stderrTail(proc))
	case text == "":
		return "", usage, errors.New("claude: no answer")
	}
	return text, usage, nil
}

func (p *CLISubprocessProvider) codexOneShot(ctx context.Context, system, prompt string) (string, error) {
	f, err := os.CreateTemp("", "codex-oneshot-*.txt")
	if err != nil {
		return "", err
	}
	f.Close()
	defer os.Remove(f.Name())
	args := []string{"exec", "--ephemeral", "--skip-git-repo-check", "-s", "read-only", "--ignore-rules",
		"-c", `model_reasoning_effort="low"`, "-o", f.Name()}
	proc, err := p.start()(ctx, "codex", args, os.TempDir())
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprint(proc.stdin, system+"\n\n---\n\n"+prompt)
	_ = proc.stdin.Close()
	for proc.lines.Scan() { // the progress it prints; the answer is in the file
	}
	if err := proc.wait(); err != nil {
		return "", fmt.Errorf("codex: %w%s", err, stderrTail(proc))
	}
	b, err := os.ReadFile(f.Name())
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("codex: no answer")
	}
	return strings.TrimSpace(string(b)), nil
}

func stderrTail(proc *nativeProc) string {
	if proc.stderr == nil {
		return ""
	}
	if s := strings.TrimSpace(proc.stderr()); s != "" {
		return ": " + truncateTail(s, 300)
	}
	return ""
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// GenerateTitle implements agent.TitleGenerator through OneShot, so a CLI agent's chats get a
// real title instead of the start of the first prompt.
func (p *CLISubprocessProvider) GenerateTitle(ctx context.Context, prompt string) (string, error) {
	text, _, err := p.OneShot(ctx, titleSystemPrompt, prompt)
	return text, err
}
