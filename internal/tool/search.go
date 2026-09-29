package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"yoakeland/internal/normalizer"
)

// SearchTool finds files and lines in the project without a shell, so the
// read-only modes (plan, read_only, explore subagents) can navigate a
// codebase instead of reading whole files to find one function.
type SearchTool struct {
	normalizer *normalizer.Normalizer
}

func NewSearchTool(norm *normalizer.Normalizer) *SearchTool {
	return &SearchTool{normalizer: norm}
}

type searchArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    string `json:"glob"`
}

const (
	maxSearchResults  = 200
	maxSearchLineLen  = 200
	maxSearchFileSize = 1 << 20
)

func (t *SearchTool) Name() string { return "search" }

func (t *SearchTool) Description() string {
	return "Search project files. With a pattern (a Go regular expression), returns matching lines as path:line: text. " +
		"Without one, lists file paths. Narrow it with path (a directory or file) and glob (a file-name pattern like *.go). " +
		"Hidden directories and node_modules are skipped; at most 200 results."
}

func (t *SearchTool) RequiresPermission(_ []byte) bool { return false }

func (t *SearchTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: t.Description(), Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Regular expression to find in file contents. Omit to list files."},
			"path":    map[string]any{"type": "string", "description": "Directory or file to search, relative to the project root. Defaults to the whole project."},
			"glob":    map[string]any{"type": "string", "description": "Only files whose name matches this pattern, e.g. *_test.go."},
		},
	}}
}

func (t *SearchTool) Execute(ctx context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args searchArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	var re *regexp.Regexp
	if args.Pattern != "" {
		var err error
		if re, err = regexp.Compile(args.Pattern); err != nil {
			return "", fmt.Errorf("invalid pattern: %w", err)
		}
	}
	if args.Glob != "" {
		if _, err := filepath.Match(args.Glob, ""); err != nil {
			return "", fmt.Errorf("invalid glob: %w", err)
		}
	}
	rootPath := "."
	if args.Path != "" {
		rootPath = args.Path
	}
	start, err := t.normalizer.ResolveAndValidatePath(rootPath)
	if err != nil {
		return "", fmt.Errorf("path error: %w", err)
	}

	hidden := normalizer.HiddenIn(t.normalizer.ProjectRoot)
	var out []string
	more := false
	// Walked through an os.Root (see normalizer.InRoot): a directory swapped
	// for a symlink mid-walk can't lead the walk or a read out of the project.
	walkErr := normalizer.InRoot(t.normalizer.ProjectRoot, start, func(r *os.Root, startRel string) error {
		fsys := r.FS()
		return fs.WalkDir(fsys, filepath.ToSlash(startRel), func(p string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return ctx.Err()
			}
			abs := filepath.Join(t.normalizer.ProjectRoot, filepath.FromSlash(p))
			return t.visit(fsys, abs, p, d, start, hidden, re, args.Glob, &out, &more)
		})
	})
	if walkErr != nil {
		return "", walkErr
	}
	if len(out) == 0 {
		return "no matches", nil
	}
	if more {
		out = append(out, fmt.Sprintf("[stopped at %d results — narrow the search with path, glob or a more specific pattern]", maxSearchResults))
	}
	return strings.Join(out, "\n"), nil
}

// visit handles one walk entry: p is its slash path inside the root fsys,
// abs its host path, start the host path the walk began at.
func (t *SearchTool) visit(fsys fs.FS, abs, p string, d fs.DirEntry, start string, hidden []string, re *regexp.Regexp, glob string, out *[]string, more *bool) error {
	if d.IsDir() {
		// hidden: yoakeland's own state (normalizer.Hidden), which isn't
		// always dot-named (~/Library/Application Support/yoake, %AppData%).
		if abs != start && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || slices.Contains(hidden, abs)) {
			return filepath.SkipDir
		}
		return nil
	}
	// Symlinks are skipped, not followed: one could point outside the
	// project.
	if !d.Type().IsRegular() {
		return nil
	}
	if glob != "" {
		if ok, _ := filepath.Match(glob, d.Name()); !ok {
			return nil
		}
	}
	rel := filepath.FromSlash(p)
	if re == nil {
		if len(*out) == maxSearchResults {
			*more = true
			return filepath.SkipAll
		}
		*out = append(*out, rel)
		return nil
	}
	if info, err := d.Info(); err != nil || info.Size() > maxSearchFileSize {
		return nil
	}
	data, err := fs.ReadFile(fsys, p)
	if err != nil || bytes.IndexByte(data[:min(len(data), 512)], 0) >= 0 {
		return nil // unreadable or binary
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), maxSearchFileSize)
	for line := 1; sc.Scan(); line++ {
		if !re.Match(sc.Bytes()) {
			continue
		}
		if len(*out) == maxSearchResults {
			*more = true
			return filepath.SkipAll
		}
		text := strings.TrimSpace(sc.Text())
		if len(text) > maxSearchLineLen {
			text = text[:maxSearchLineLen] + "…"
		}
		*out = append(*out, fmt.Sprintf("%s:%d: %s", rel, line, text))
	}
	return nil
}
