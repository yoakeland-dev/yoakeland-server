package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"yoakeland/internal/diff"
	"yoakeland/internal/normalizer"
)

type FileEditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"oldString"`
	NewString  string `json:"newString"`
	ReplaceAll bool   `json:"replaceAll,omitempty"`
}

type FileEditTool struct {
	normalizer *normalizer.Normalizer
}

func NewFileEditTool(norm *normalizer.Normalizer) *FileEditTool {
	return &FileEditTool{normalizer: norm}
}

func (t *FileEditTool) Name() string {
	return "file_edit"
}

func (t *FileEditTool) Description() string {
	return "Perform targeted text replacement on a file in the project. Triggers an automatic snapshot."
}

func (t *FileEditTool) RequiresPermission(_ []byte) bool {
	return true
}

func (t *FileEditTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args FileEditArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.OldString == "" {
		return "", fmt.Errorf("oldString cannot be empty")
	}

	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}
	if execCtx.ApprovedPath != "" && safePath != execCtx.ApprovedPath {
		return "", ErrPathChanged
	}
	if t.normalizer.IsGitExecPath(safePath) {
		return "", normalizer.ErrGitExecPath
	}

	contentBytes, err := readInProject(t.normalizer, safePath)
	if err != nil {
		return "", fmt.Errorf("failed reading file to edit: %w", err)
	}
	content := string(contentBytes)

	occurrences := strings.Count(content, args.OldString)
	if occurrences == 0 {
		return "", fmt.Errorf("target oldString not found in %s", args.Path)
	}
	if occurrences > 1 && !args.ReplaceAll {
		return "", fmt.Errorf("oldString appears %d times in %s; provide more context or set replaceAll: true", occurrences, args.Path)
	}

	// Capture snapshot before editing
	if err := capturePreMutationSnapshot(execCtx, "file_edit: "+args.Path, t.normalizer.ProjectRoot, safePath); err != nil {
		return "", fmt.Errorf("failed to create snapshot before edit: %w", err)
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := WriteFileAtomic(t.normalizer.ProjectRoot, safePath, []byte(updated)); err != nil {
		return "", fmt.Errorf("failed writing edit to disk: %w", err)
	}

	return fmt.Sprintf("Replaced %d occurrence(s) in %s", occurrences, args.Path), nil
}

func (t *FileEditTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Target file to edit.",
				},
				"oldString": map[string]any{
					"type":        "string",
					"description": "Exact text to locate and replace.",
				},
				"newString": map[string]any{
					"type":        "string",
					"description": "Replacement text.",
				},
				"replaceAll": map[string]any{
					"type":        "boolean",
					"description": "Whether to replace all occurrences if multiple matches exist.",
				},
			},
			"required": []string{"path", "oldString", "newString"},
		},
	}
}

func (t *FileEditTool) PreviewDiff(argsJSON []byte) (*diff.FileDiff, error) {
	var args FileEditArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, err
	}

	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if err != nil {
		return nil, err
	}

	existingBytes, err := readInProject(t.normalizer, safePath)
	if err != nil {
		return nil, err
	}
	beforeContent := string(existingBytes)

	var afterContent string
	if args.ReplaceAll {
		afterContent = strings.ReplaceAll(beforeContent, args.OldString, args.NewString)
	} else {
		afterContent = strings.Replace(beforeContent, args.OldString, args.NewString, 1)
	}

	return diff.ComputeDiff(args.Path, beforeContent, afterContent), nil
}
