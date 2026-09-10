package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"yoakeland/internal/diff"
	"yoakeland/internal/normalizer"
)

type FileWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type FileWriteTool struct {
	normalizer *normalizer.Normalizer
}

func NewFileWriteTool(norm *normalizer.Normalizer) *FileWriteTool {
	return &FileWriteTool{normalizer: norm}
}

func (t *FileWriteTool) Name() string {
	return "file_write"
}

func (t *FileWriteTool) Description() string {
	return "Create or overwrite a file in the project. Triggers an automatic snapshot."
}

func (t *FileWriteTool) RequiresPermission(_ []byte) bool {
	return true
}

func (t *FileWriteTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args FileWriteArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}

	// Automatic snapshot before mutation
	if execCtx.SnapshotStore != nil {
		_, err := execCtx.SnapshotStore.CapturePreMutation(
			"file_write: "+args.Path,
			t.normalizer.ProjectRoot,
			safePath,
		)
		if err != nil {
			return "", fmt.Errorf("failed to create snapshot before write: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(safePath), 0755); err != nil {
		return "", err
	}

	// Atomic write via temp file
	tmpPath := safePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write buffer: %w", err)
	}
	if err := os.Rename(tmpPath, safePath); err != nil {
		return "", fmt.Errorf("failed to commit file write: %w", err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path), nil
}

func (t *FileWriteTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File destination path relative to project root.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file content to write.",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (t *FileWriteTool) PreviewDiff(argsJSON []byte) (*diff.FileDiff, error) {
	var args FileWriteArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, err
	}

	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if err != nil {
		return nil, err
	}

	var beforeContent string
	if existingBytes, err := os.ReadFile(safePath); err == nil {
		beforeContent = string(existingBytes)
	}

	return diff.ComputeDiff(args.Path, beforeContent, args.Content), nil
}
