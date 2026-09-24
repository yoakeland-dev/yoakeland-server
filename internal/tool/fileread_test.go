package tool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/tool"
)

func TestFileReadTool_WholeFileUnchangedWithoutLineArgs(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	rt := tool.NewFileReadTool(norm)

	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(filepath.Join(tempDir, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(tool.FileReadArgs{Path: "f.txt"})
	out, err := rt.Execute(context.Background(), tool.ExecutionContext{}, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != content {
		t.Fatalf("expected the whole file unchanged, got %q", out)
	}
}

func TestFileReadTool_LineRange(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	rt := tool.NewFileReadTool(norm)

	content := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(filepath.Join(tempDir, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		start, end int
		want       string
	}{
		{"middle range", 2, 4, "line2\nline3\nline4"},
		{"start only, to end", 3, 0, "line3\nline4\nline5"},
		{"end only, from start", 0, 2, "line1\nline2"},
		{"single line", 3, 3, "line3"},
		{"end beyond total clamps", 4, 100, "line4\nline5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, _ := json.Marshal(tool.FileReadArgs{Path: "f.txt", StartLine: c.start, EndLine: c.end})
			out, err := rt.Execute(context.Background(), tool.ExecutionContext{}, args)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if out != c.want {
				t.Fatalf("got %q, want %q", out, c.want)
			}
		})
	}
}

func TestFileReadTool_LineRangeErrors(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	rt := tool.NewFileReadTool(norm)

	content := "line1\nline2\nline3"
	if err := os.WriteFile(filepath.Join(tempDir, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		start, end int
	}{
		{"start after end", 3, 1},
		{"start past last line", 10, 0},
		{"negative start", -1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, _ := json.Marshal(tool.FileReadArgs{Path: "f.txt", StartLine: c.start, EndLine: c.end})
			if _, err := rt.Execute(context.Background(), tool.ExecutionContext{}, args); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestFileReadTool_AttachedPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	norm, _ := normalizer.New(root)
	rt := tool.NewFileReadTool(norm)

	attachedDir := filepath.Join(outside, "shared")
	_ = os.Mkdir(attachedDir, 0o755)
	_ = os.WriteFile(filepath.Join(outside, "a.txt"), []byte("A"), 0o644)
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("S"), 0o644)
	_ = os.WriteFile(filepath.Join(attachedDir, "b.txt"), []byte("B"), 0o644)
	_ = os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(attachedDir, "escape"))

	ctx := tool.ExecutionContext{ExtraReadPaths: []string{filepath.Join(outside, "a.txt"), attachedDir}}
	read := func(p string) (string, error) {
		args, _ := json.Marshal(tool.FileReadArgs{Path: p})
		return rt.Execute(context.Background(), ctx, args)
	}

	if out, err := read(filepath.Join(outside, "a.txt")); err != nil || out != "A" {
		t.Fatalf("attached file: %q, %v", out, err)
	}
	if out, err := read(filepath.Join(attachedDir, "b.txt")); err != nil || out != "B" {
		t.Fatalf("file under attached dir: %q, %v", out, err)
	}
	if _, err := read(filepath.Join(outside, "secret.txt")); err == nil {
		t.Fatal("unattached sibling must stay unreadable")
	}
	if _, err := read(filepath.Join(attachedDir, "escape")); err == nil {
		t.Fatal("symlink out of an attached dir must stay unreadable")
	}
}
