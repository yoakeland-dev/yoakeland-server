package tool

import (
	"context"
	"encoding/json"
	"fmt"
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
	return "Execute a command inside the project sandbox."
}

func (t *BashTool) RequiresPermission(_ []byte) bool {
	return true
}

func (t *BashTool) Execute(ctx context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args BashArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	parsed, err := t.normalizer.ParseShellArgv(args.Command)
	if err != nil {
		return "", fmt.Errorf("command normalization failed: %w", err)
	}

	res, err := t.sandbox.Execute(ctx, parsed.Argv)
	if err != nil {
		return "", fmt.Errorf("sandbox error: %w", err)
	}

	if res.ExitCode != 0 {
		return fmt.Sprintf("Command failed (exit code %d)\nSTDOUT:\n%s\nSTDERR:\n%s",
			res.ExitCode, res.Stdout, res.Stderr), nil
	}

	return res.Stdout, nil
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
