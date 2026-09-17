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
	if err := capturePreMutationSnapshot(execCtx, "file_write: "+args.Path, t.normalizer.ProjectRoot, safePath); err != nil {
		return "", fmt.Errorf("failed to create snapshot before write: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(safePath), 0755); err != nil {
		return "", err
	}

	// Atomic write via temp file. Opened with O_NOFOLLOW so a symlink
	// planted at tmpPath between path validation and this write (e.g. by a
	// concurrent tool call or a malicious repo checkout) is refused instead
	// of silently followed and written through to wherever it points.
	tmpPath := safePath + ".tmp"
	f, err := openNoFollow(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to write buffer: %w", err)
	}
	_, writeErr := f.Write([]byte(args.Content))
	closeErr := f.Close()
	if writeErr != nil {
		return "", fmt.Errorf("failed to write buffer: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("failed to write buffer: %w", closeErr)
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
