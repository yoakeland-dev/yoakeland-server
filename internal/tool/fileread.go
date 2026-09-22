package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
)

type FileReadArgs struct {
	Path string `json:"path"`

	// StartLine/EndLine (1-indexed, inclusive) request a snippet instead of
	// the whole file — for a large file where only a known section matters.
	// Zero means "unset": StartLine 0 defaults to 1, EndLine 0 defaults to
	// the last line. Giving neither reads the whole file unchanged (today's
	// behavior).
	StartLine int `json:"startLine,omitempty"`
	EndLine   int `json:"endLine,omitempty"`
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
	return "Read file contents within the project root directory. Optionally pass startLine/endLine to read a snippet of a large file instead of the whole thing."
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

	info, err := os.Stat(safePath)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}
	if info.Size() > protocol.MaxFrameSize {
		return "", fmt.Errorf("file exceeds maximum readable size (10MB)")
	}

	content, err := os.ReadFile(safePath)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	if args.StartLine == 0 && args.EndLine == 0 {
		return string(content), nil
	}
	return sliceLines(string(content), args.StartLine, args.EndLine)
}

// sliceLines returns the 1-indexed, inclusive [startLine, endLine] range of
// content — startLine 0 means "from the start", endLine 0 means "to the
// end". A file ending in "\n" splits into a trailing empty element (same as
// strings.Split always does); that counts as the file's last line for
// range-checking purposes, matching what a 1-indexed editor view shows.
func sliceLines(content string, startLine, endLine int) (string, error) {
	if startLine < 0 || endLine < 0 {
		return "", fmt.Errorf("startLine/endLine must not be negative")
	}
	lines := strings.Split(content, "\n")
	total := len(lines)

	start := startLine
	if start == 0 {
		start = 1
	}
	end := endLine
	if end == 0 {
		end = total
	}
	if start > end {
		return "", fmt.Errorf("startLine (%d) must not be after endLine (%d)", start, end)
	}
	if start > total {
		return "", fmt.Errorf("startLine %d is past the file's last line (%d)", start, total)
	}
	if end > total {
		end = total
	}
	return strings.Join(lines[start-1:end], "\n"), nil
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
				"startLine": map[string]any{
					"type":        "integer",
					"description": "1-indexed line to start reading from (inclusive). Omit to read from the start of the file.",
				},
				"endLine": map[string]any{
					"type":        "integer",
					"description": "1-indexed line to stop reading at (inclusive). Omit to read to the end of the file.",
				},
			},
			"required": []string{"path"},
		},
	}
}
