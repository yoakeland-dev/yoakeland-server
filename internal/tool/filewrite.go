package tool

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"yoakeland/internal/diff"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
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
	if execCtx.ApprovedPath != "" && safePath != execCtx.ApprovedPath {
		return "", ErrPathChanged
	}
	if t.normalizer.IsGitExecPath(safePath) {
		return "", normalizer.ErrGitExecPath
	}

	// Automatic snapshot before mutation
	if err := capturePreMutationSnapshot(execCtx, "file_write: "+args.Path, t.normalizer.ProjectRoot, safePath); err != nil {
		return "", fmt.Errorf("failed to create snapshot before write: %w", err)
	}

	if err := WriteFileAtomic(t.normalizer.ProjectRoot, safePath, []byte(args.Content)); err != nil {
		return "", err
	}

	return fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), args.Path), nil
}

// WriteFileAtomic replaces path's content (path inside root, already
// validated) via a temp file in the same directory and a rename, so a reader
// never sees a half-written file. Everything goes through an os.Root at root
// (normalizer.InRoot), so a directory swapped for a symlink after validation
// can't redirect the write out of the project; the temp file is created
// exclusively under a random name, so a symlink planted at it is never
// written through. An existing file keeps its mode.
func WriteFileAtomic(root, path string, data []byte) error {
	return normalizer.InRoot(root, path, func(r *os.Root, rel string) error {
		mode := os.FileMode(0644)
		if fi, err := r.Stat(rel); err == nil {
			mode = fi.Mode().Perm()
		}
		dir := filepath.Dir(rel)
		if err := r.MkdirAll(dir, 0755); err != nil {
			return err
		}
		tmp := filepath.Join(dir, "."+filepath.Base(rel)+"."+rand.Text()+".tmp")
		f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("failed to write buffer: %w", err)
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Chmod(mode)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = r.Rename(tmp, rel)
		}
		if err != nil {
			_ = r.Remove(tmp)
			return fmt.Errorf("failed to write %s: %w", filepath.Base(rel), err)
		}
		return nil
	})
}

// readInProject reads abs (validated, inside n's root) through an os.Root —
// see WriteFileAtomic — refusing anything but a regular file, so a FIFO or
// device node can't hang the turn.
func readInProject(n *normalizer.Normalizer, abs string) ([]byte, error) {
	var data []byte
	err := normalizer.InRoot(n.ProjectRoot, abs, func(r *os.Root, rel string) error {
		var err error
		data, err = readRegular(r.Stat, r.ReadFile, rel)
		return err
	})
	return data, err
}

func readRegular(stat func(string) (os.FileInfo, error), read func(string) ([]byte, error), p string) ([]byte, error) {
	info, err := stat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	if info.Size() > protocol.MaxFrameSize {
		return nil, fmt.Errorf("file exceeds maximum readable size (10MB)")
	}
	return read(p)
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
	if existingBytes, err := readInProject(t.normalizer, safePath); err == nil {
		beforeContent = string(existingBytes)
	}

	return diff.ComputePreview(args.Path, beforeContent, args.Content), nil
}
