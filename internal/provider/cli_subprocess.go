package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type CLIKind string

const (
	CLIKindClaude CLIKind = "claude"
	CLIKindCodex  CLIKind = "codex"
)

// disallowedClaudeTools lists every built-in Claude Code tool that can
// mutate the project or spawn further agent work. Passed to
// --disallowedTools so the CLI is reduced to pure text completion plus
// (Anthropic-)server-side web search: yoakeland keeps sole ownership of
// file/shell access through its own permission-gated tool engine, rather
// than letting the CLI mutate the project on its own. WebFetch/WebSearch
// are deliberately left enabled — they're read-only and run server-side on
// Claude's own account, so this gets working web search with no extra
// setup instead of reimplementing it client-side.
const disallowedClaudeTools = "Bash,Read,Write,Edit,Glob,Grep,Task,NotebookEdit,TodoWrite,BashOutput,KillShell,SlashCommand"

// commandRunner abstracts process execution so tests can substitute a fake
// binary instead of requiring the real `claude`/`codex` CLI to be installed.
type commandRunner func(ctx context.Context, name string, args []string, dir string) ([]byte, error)

// runCommand returns whatever stdout the process produced even when it
// exits non-zero: both CLIs report turn-level failures (expired auth, usage
// limits, model errors) as a normal JSON/JSONL result on stdout paired with
// exit code 1, so the caller needs that output to parse the real reason —
// discarding it on a non-zero exit leaves only the generic wrapper error.
func runCommand(ctx context.Context, name string, args []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("%s exited with %v: %s", name, err, string(exitErr.Stderr))
		}
		return out, err
	}
	return out, nil
}

// CLISubprocessProvider drives the official `claude` or `codex` CLI as a
// subprocess instead of calling the model API directly, so usage is billed
// to the user's own subscription. It behaves as a pure ModelProvider: all
// of the CLI's own built-in tool use is disabled or sandboxed away, and its
// output is fed back into yoakeland's normal Agent loop like any other
// provider.
//
// Streaming caveat: unlike the HTTP providers, StreamTurn here does not
// emit token-by-token deltas. Each invocation is a one-shot subprocess run
// to completion (`claude -p --output-format json` / `codex exec --json`),
// buffered and parsed only after it exits — see parseClaudeOutput and
// parseCodexOutput below. The frontend gets a single StreamChunkDelta
// carrying the whole response, not incremental chunks. A frontend that
// wants real incremental streaming here would need the CLI's own
// partial-message JSON stream (e.g. `claude --output-format stream-json`)
// wired through; that's unverified surface area, not implemented here — see
// SPEC.md §6b.
//
// systemPrompt (mode-selected by Agent, see agent.activeSystemPrompt) is
// accepted but ignored here: the CLI subprocess owns its own baked-in
// system prompt end-to-end, this process has no hook to override it.
type CLISubprocessProvider struct {
	Kind  CLIKind
	Model string
	Dir   string
	Run   commandRunner // nil uses the real subprocess runner
}

func (p *CLISubprocessProvider) ModelName() string { return p.Model }

func (p *CLISubprocessProvider) run() commandRunner {
	if p.Run != nil {
		return p.Run
	}
	return runCommand
}

func (p *CLISubprocessProvider) StreamTurn(ctx context.Context, history []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 2)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		prompt := renderCLIPrompt(history)

		var binary string
		var args []string
		switch p.Kind {
		case CLIKindClaude:
			binary = "claude"
			args = claudeArgs(p.Model, prompt)
		case CLIKindCodex:
			binary = "codex"
			args = codexArgs(p.Model, prompt)
		default:
			eChan <- fmt.Errorf("unknown CLI subprocess kind %q", p.Kind)
			return
		}

		out, runErr := p.run()(ctx, binary, args, p.Dir)

		// A non-zero exit doesn't mean stdout is useless: try parsing it
		// first (see runCommand) and only fall back to the raw exec error
		// — which for these CLIs is often just stderr noise — when there's
		// nothing on stdout to parse.
		var text string
		var usage agent.ModelStreamChunk
		var parseErr error
		switch p.Kind {
		case CLIKindClaude:
			text, usage, parseErr = parseClaudeOutput(out)
		case CLIKindCodex:
			text, usage, parseErr = parseCodexOutput(out)
		}
		if parseErr != nil {
			// Nothing on stdout to parse at all (binary missing, killed
			// before printing anything) — the exec-level error is all
			// there is. Otherwise parseErr already carries the CLI's own
			// reported reason (auth failure, usage limit, ...); prefer it
			// over the generic wrapped exec error.
			if len(bytes.TrimSpace(out)) == 0 && runErr != nil {
				eChan <- fmt.Errorf("%s subprocess failed: %w", binary, runErr)
			} else {
				eChan <- parseErr
			}
			return
		}

		cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text}
		cChan <- usage
	}()

	return cChan, eChan
}

func claudeArgs(model, prompt string) []string {
	args := []string{"-p", "--output-format", "json", "--disallowedTools", disallowedClaudeTools}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "--", prompt)
}

func codexArgs(model, prompt string) []string {
	// --sandbox read-only: same reasoning as --disallowedTools above — the
	// CLI must not mutate the project on its own, yoakeland's tool engine
	// does that. ponytail: pure text-completion use, not full agent delegation.
	// tools.web_search=true turns on Codex's own server-side web search
	// (read-only, runs on the same ChatGPT/OpenAI account already in use)
	// so search works without a separate API key.
	args := []string{"exec", "--json", "--sandbox", "read-only", "-c", "tools.web_search=true"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "--", prompt)
}

// renderCLIPrompt flattens yoakeland's structured turn history into the
// single prompt string these one-shot CLI invocations take, since each
// invocation is a fresh process with no notion of yoakeland's own history.
func renderCLIPrompt(history []agent.Message) string {
	var sb strings.Builder
	for _, m := range history {
		switch m.Role {
		case "user":
			sb.WriteString("User: ")
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		case "assistant":
			if m.Content != "" {
				sb.WriteString("Assistant: ")
				sb.WriteString(m.Content)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

type claudeCLIResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// parseClaudeOutput reads the single JSON object `claude -p --output-format
// json` prints on completion.
func parseClaudeOutput(out []byte) (string, agent.ModelStreamChunk, error) {
	trimmed := bytes.TrimSpace(out)
	var res claudeCLIResult
	if err := json.Unmarshal(trimmed, &res); err != nil {
		return "", agent.ModelStreamChunk{}, fmt.Errorf("parsing claude CLI output: %w", err)
	}
	if res.IsError {
		return "", agent.ModelStreamChunk{}, fmt.Errorf("claude CLI reported an error: %s", res.Result)
	}

	usage := &protocol.UsageData{
		InputTokens:  res.Usage.InputTokens,
		OutputTokens: res.Usage.OutputTokens,
		TotalTokens:  res.Usage.InputTokens + res.Usage.OutputTokens,
		CostUSD:      res.TotalCostUSD,
		CostKnown:    true, // claude CLI always reports total_cost_usd, even when it's 0
	}
	return res.Result, agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: usage}, nil
}

type codexCLIEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"` // set on "turn.failed" events
}

// parseCodexOutput reads the JSON-Lines event stream `codex exec --json`
// prints: the final assistant message comes from an item.completed event of
// type agent_message, token usage from turn.completed.
func parseCodexOutput(out []byte) (string, agent.ModelStreamChunk, error) {
	var text string
	found := false
	usage := &protocol.UsageData{}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev codexCLIEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // tolerate stray non-JSON lines on stdout
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				text = ev.Item.Text
				found = true
			}
		case "turn.completed":
			if ev.Usage != nil {
				usage.InputTokens = ev.Usage.InputTokens
				usage.OutputTokens = ev.Usage.OutputTokens
				usage.TotalTokens = ev.Usage.InputTokens + ev.Usage.OutputTokens
			}
		case "turn.failed":
			if ev.Error != nil && ev.Error.Message != "" {
				return "", agent.ModelStreamChunk{}, fmt.Errorf("codex CLI turn failed: %s", ev.Error.Message)
			}
			return "", agent.ModelStreamChunk{}, fmt.Errorf("codex CLI turn failed")
		}
	}
	if !found {
		return "", agent.ModelStreamChunk{}, fmt.Errorf("no agent_message found in codex CLI output")
	}
	return text, agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: usage}, nil
}
