package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/sandbox"
)

type BashArgs struct {
	Command string `json:"command"`
}

type BashTool struct {
	normalizer *normalizer.Normalizer
	sandbox    sandbox.Sandbox
}

func NewBashTool(norm *normalizer.Normalizer, sb sandbox.Sandbox) *BashTool {
	return &BashTool{
		normalizer: norm,
		sandbox:    sb,
	}
}

func (t *BashTool) Name() string {
	return "bash"
}

func (t *BashTool) Description() string {
	return "Execute a single command inside the project sandbox. There is no shell: " +
		"pipes (|), redirects (>, <), command chaining (;, &&, ||), backticks, and $() " +
		"are not interpreted — they're passed through as literal characters, so a command " +
		"using them fails or behaves unexpectedly instead of working as in a real shell. " +
		"Run one command per call and chain multiple calls instead of piping."
}

func (t *BashTool) RequiresPermission(_ []byte) bool {
	return true
}

func (t *BashTool) Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args BashArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	parsed, err := t.normalizer.ParseShellArgv(args.Command)
	if err != nil {
		return "", fmt.Errorf("command normalization failed: %w", err)
	}

	res, err := t.sandbox.Execute(ctx, parsed.Argv, gitAuthorEnv(execCtx))
	if err != nil {
		return "", fmt.Errorf("sandbox error: %w", err)
	}

	if res.ExitCode != 0 {
		return fmt.Sprintf("Command failed (exit code %d)\nSTDOUT:\n%s\nSTDERR:\n%s",
			res.ExitCode, wrapCommandOutput(res.Stdout), wrapCommandOutput(res.Stderr)), nil
	}

	return wrapCommandOutput(res.Stdout), nil
}

// wrapCommandOutput marks what a command printed as data: it can carry text
// from anywhere (curl, a cloned file, a dev server's log), so it gets the same
// untrusted-content framing as web pages. Empty output stays empty.
func wrapCommandOutput(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return WrapUntrusted("a command's output", s)
}

// gitAuthorEnv returns GIT_AUTHOR_*/GIT_COMMITTER_* env vars for whichever
// peer's prompt started this turn, or nil when unknown (a unix peer with no
// registered token) — in which case the sandbox's ambient git config, if
// any, applies unchanged. Set unconditionally rather than only for commands
// that look like `git commit`: harmless for anything else, and covers
// `git commit --amend`, scripted commits, etc. without a detection
// heuristic.
func gitAuthorEnv(execCtx ExecutionContext) []string {
	if execCtx.AuthorName == "" {
		return nil
	}
	email := execCtx.AuthorEmail
	if email == "" {
		email = strings.ToLower(strings.ReplaceAll(execCtx.AuthorName, " ", ".")) + "@yoake.local"
	}
	return []string{
		"GIT_AUTHOR_NAME=" + execCtx.AuthorName,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + execCtx.AuthorName,
		"GIT_COMMITTER_EMAIL=" + email,
	}
}

func (t *BashTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command line to run inside the sandbox.",
				},
			},
			"required": []string{"command"},
		},
	}
}
