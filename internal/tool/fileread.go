package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"yoakeland/internal/normalizer"
)

type FileReadArgs struct {
	Path string `json:"path"`

	// StartLine/EndLine (1-indexed, inclusive) request a snippet instead of
	// the whole file — for a large file where only a known section matters.
	// Zero means "unset": StartLine 0 defaults to 1, EndLine 0 defaults to
	// the last line. Either way, at most maxReadLines/maxReadBytes come back,
	// with a note saying where to continue.
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
	return "Read a file within the project root. Each line comes back prefixed with its line number and a tab; the prefix " +
		"isn't part of the file, so leave it out of file_edit's oldString. Long files come back in pages of up to 2000 lines — " +
		"pass startLine/endLine to read a specific range, or to continue where a page stopped."
}

func (t *FileReadTool) RequiresPermission(_ []byte) bool {
	return false // Always permitted by default
}

func (t *FileReadTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args FileReadArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	var content []byte
	safePath, err := t.normalizer.ResolveAndValidatePath(args.Path)
	if errors.Is(err, normalizer.ErrEscapesRoot) {
		// A user-attached path outside the project: nothing in the sandbox
		// can write there, so plain os calls are fine.
		if safePath, err = resolveAttached(args.Path, execCtx.ExtraReadPaths, err); err == nil {
			content, err = readRegular(os.Stat, os.ReadFile, safePath)
		}
	} else if err == nil {
		content, err = readInProject(t.normalizer, safePath)
	}
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}

	return numberedLines(string(content), args.StartLine, args.EndLine)
}

// resolveAttached allows an absolute path outside the project root when it
// is, or sits under, a path the user attached. Both sides are symlink-
// resolved, so a link inside an attached folder can't point back out of it.
// rootErr is returned unchanged when nothing matches.
func resolveAttached(rawPath string, attached []string, rootErr error) (string, error) {
	if len(attached) == 0 || !filepath.IsAbs(rawPath) {
		return "", rootErr
	}
	real, err := filepath.EvalSymlinks(rawPath)
	if err != nil || normalizer.InHidden(real) {
		return "", rootErr
	}
	for _, a := range attached {
		realA, err := filepath.EvalSymlinks(a)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(realA, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", rootErr
}

const (
	maxReadLines = 2000
	// maxReadBytes stays under the agent's tool-output cap (16 KB by
	// default), so a page is never cut in the middle by that instead.
	maxReadBytes = 14000
)

// numberedLines returns the 1-indexed, inclusive [startLine, endLine] range
// of content, each line prefixed with its number — startLine 0 means "from
// the start", endLine 0 means "to the end". A page stops early at
// maxReadLines/maxReadBytes and ends with a note saying where to continue.
func numberedLines(content string, startLine, endLine int) (string, error) {
	if startLine < 0 || endLine < 0 {
		return "", fmt.Errorf("startLine/endLine must not be negative")
	}
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1] // the newline ending the last line doesn't start another
	}
	total := len(lines)

	start, end := max(startLine, 1), endLine
	if end == 0 || end > total {
		end = total
	}
	if start > total {
		return "", fmt.Errorf("startLine %d is past the file's last line (%d)", start, total)
	}
	if start > end {
		return "", fmt.Errorf("startLine (%d) must not be after endLine (%d)", start, end)
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if i > start && (i-start >= maxReadLines || b.Len()+len(line) > maxReadBytes) {
			fmt.Fprintf(&b, "[showing lines %d-%d of %d; pass startLine: %d to read on]", start, i-1, total, i)
			return b.String(), nil
		}
		if len(line) > maxReadBytes {
			line = line[:maxReadBytes] + "…"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
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
