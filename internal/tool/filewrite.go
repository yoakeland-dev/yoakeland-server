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

	if err := WriteFileAtomic(safePath, []byte(args.Content)); err != nil {
		return "", err
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path), nil
}

// WriteFileAtomic replaces path's content via a temp file in the same
// directory and a rename, so a reader never sees a half-written file. The
// temp file is created exclusively under a random name, so a symlink planted
// in the meantime can't redirect the write. An existing file keeps its mode.
func WriteFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to write buffer: %w", err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("failed to write %s: %w", filepath.Base(path), err)
	}
	return nil
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
