package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"yoakeland/internal/normalizer"
)

type FileReadArgs struct {
	Path string `json:"path"`
}

type FileReadTool struct {
	normalizer *normalizer.Normalizer
}

func NewFileReadTool(norm *normalizer.Normalizer) *FileReadTool {
	return &FileReadTool{normalizer: norm}
}

func (t *FileReadTool) Name() string {
	return "file_read"
}

func (t *FileReadTool) Description() string {
	return "Read file contents within the project root directory."
}

func (t *FileReadTool) RequiresPermission(_ []byte) bool {
	return false // Always permitted by default
}

func (t *FileReadTool) Execute(_ context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args FileReadArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}

	content, err := os.ReadFile(safePath)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	return string(content), nil
}

func (t *FileReadTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The path of the file to read relative to the project root.",
				},
			},
			"required": []string{"path"},
		},
	}
}
