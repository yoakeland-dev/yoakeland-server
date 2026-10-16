package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/tool"
)

func TestFileReadTool_WholeFileNumberedWithoutLineArgs(t *testing.T) {
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
	if want := "     1\tline1\n     2\tline2\n     3\tline3"; out != want {
		t.Fatalf("got %q, want %q", out, want)
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
		{"middle range", 2, 4, "     2\tline2\n     3\tline3\n     4\tline4"},
		{"start only, to end", 3, 0, "     3\tline3\n     4\tline4\n     5\tline5"},
		{"end only, from start", 0, 2, "     1\tline1\n     2\tline2"},
		{"single line", 3, 3, "     3\tline3"},
		{"end beyond total clamps", 4, 100, "     4\tline4\n     5\tline5"},
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

	if out, err := read(filepath.Join(outside, "a.txt")); err != nil || out != "     1\tA" {
		t.Fatalf("attached file: %q, %v", out, err)
	}
	if out, err := read(filepath.Join(attachedDir, "b.txt")); err != nil || out != "     1\tB" {
		t.Fatalf("file under attached dir: %q, %v", out, err)
	}
	if _, err := read(filepath.Join(outside, "secret.txt")); err == nil {
		t.Fatal("unattached sibling must stay unreadable")
	}
	if _, err := read(filepath.Join(attachedDir, "escape")); err == nil {
		t.Fatal("symlink out of an attached dir must stay unreadable")
	}
}

// A long file comes back one page at a time, ending with where to go on.
func TestFileReadTool_PagesLongFiles(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	rt := tool.NewFileReadTool(norm)

	var content string
	for i := 0; i < 2500; i++ {
		content += "x\n"
	}
	_ = os.WriteFile(filepath.Join(tempDir, "big.txt"), []byte(content), 0644)

	args, _ := json.Marshal(tool.FileReadArgs{Path: "big.txt"})
	out, err := rt.Execute(context.Background(), tool.ExecutionContext{}, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasSuffix(out, "to read on]") || !strings.Contains(out, "of 2500; pass startLine:") || strings.Contains(out, "  2500\t") {
		t.Fatalf("expected one page ending in a continuation note, got tail %q", out[len(out)-80:])
	}
}

func TestSearchTool(t *testing.T) {
	root := t.TempDir()
	norm, _ := normalizer.New(root)
	st := tool.NewSearchTool(norm)
	_ = os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\nfunc Wanted() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pkg", "b.txt"), []byte("func Wanted too\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".git", "c.go"), []byte("func Wanted() {}\n"), 0o644)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret.go"), []byte("func Wanted() {}\n"), 0o644)
	_ = os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "pkg", "link.go"))

	run := func(args string) string {
		out, err := st.Execute(context.Background(), tool.ExecutionContext{}, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		return out
	}
	if got, want := run(`{"pattern":"func Wanted","glob":"*.go"}`), "pkg/a.go:2: func Wanted() {}"; got != want {
		t.Fatalf("grep: got %q, want %q (no hidden dirs, no symlinks, glob applied)", got, want)
	}
	if got, want := run(`{"path":"pkg"}`), "pkg/a.go\npkg/b.txt"; got != want {
		t.Fatalf("list: got %q, want %q", got, want)
	}
	if _, err := st.Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"path":"../"}`)); err == nil {
		t.Fatal("a path outside the project must be refused")
	}
}

// search never walks into yoakeland's own state, even when it isn't
// dot-named (macOS/Windows defaults) and the project contains it.
func TestSearch_SkipsHiddenStateDir(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "Library", "yoake")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "server.json"), []byte(`{"token":"SECRET-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("// SECRET-TOKEN is not here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := normalizer.Hidden
	normalizer.Hidden = []string{state}
	defer func() { normalizer.Hidden = old }()
	norm, _ := normalizer.New(root)
	out, err := tool.NewSearchTool(norm).Execute(context.Background(), tool.ExecutionContext{}, []byte(`{"pattern":"SECRET-TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "server.json") || !strings.Contains(out, "main.go:1:") {
		t.Fatalf("search output:\n%s", out)
	}
}

// Yoakeland's state dir can't be read through an attached folder, the way
// file_read refuses it inside the project; the upload store under it can.
func TestFileReadTool_AttachedStateDirRefused(t *testing.T) {
	root, _ := normalizer.CanonicalRoot(t.TempDir())
	state := filepath.Join(root, ".yoake")
	uploads := filepath.Join(state, "uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(state, "server.json"), []byte("token"), 0o600)
	_ = os.WriteFile(filepath.Join(uploads, "u.txt"), []byte("U"), 0o600)
	oldHidden, oldUploads := normalizer.Hidden, normalizer.Uploads
	normalizer.Hidden, normalizer.Uploads = []string{state}, uploads
	t.Cleanup(func() { normalizer.Hidden, normalizer.Uploads = oldHidden, oldUploads })
	norm, _ := normalizer.New(root)
	rt := tool.NewFileReadTool(norm)

	ctx := tool.ExecutionContext{ExtraReadPaths: []string{state}}
	read := func(p string) (string, error) {
		args, _ := json.Marshal(tool.FileReadArgs{Path: p})
		return rt.Execute(context.Background(), ctx, args)
	}
	if _, err := read(filepath.Join(state, "server.json")); err == nil {
		t.Fatal("state file read through an attached folder")
	}
	if out, err := read(filepath.Join(uploads, "u.txt")); err != nil || out != "     1\tU" {
		t.Fatalf("upload: %q, %v", out, err)
	}
}

// A search walk leaves out the files ReadAllowed refuses, by name or by
// listing, so searching a folder above a secret can't surface it.
func TestSearch_SkipsRefusedFiles(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("KEY=SECRET\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "app.txt"), []byte("KEY=public\n"), 0o644)
	norm, _ := normalizer.New(root)
	ctx := tool.ExecutionContext{ReadAllowed: func(abs string) bool { return filepath.Base(abs) != ".env" }}
	out, err := tool.NewSearchTool(norm).Execute(context.Background(), ctx, []byte(`{"pattern":"KEY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".env") || !strings.Contains(out, "app.txt:1:") {
		t.Fatalf("search output:\n%s", out)
	}
}

// A symlink repointed between the permission check and the write (a sandboxed
// process did it while the user was asked) must not redirect the write.
func TestFileWriteTool_RefusesRepointedSymlink(t *testing.T) {
	root, _ := normalizer.CanonicalRoot(t.TempDir())
	_ = os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	target := filepath.Join(root, "docs", "a.txt")
	env := filepath.Join(root, ".env")
	_ = os.WriteFile(target, []byte("a"), 0o644)
	_ = os.WriteFile(env, []byte("SECRET"), 0o600)
	link := filepath.Join(root, "docs", "x")
	_ = os.Symlink(target, link)
	norm, _ := normalizer.New(root)
	wt := tool.NewFileWriteTool(norm)
	write := func(approved string) error {
		args, _ := json.Marshal(tool.FileWriteArgs{Path: "docs/x", Content: "new"})
		_, err := wt.Execute(context.Background(), tool.ExecutionContext{ApprovedPath: approved}, args)
		return err
	}

	if err := write(target); err != nil {
		t.Fatalf("approved write: %v", err)
	}
	_ = os.Remove(link)
	_ = os.Symlink(env, link)
	if err := write(target); !errors.Is(err, tool.ErrPathChanged) {
		t.Fatalf("repointed write: %v, want ErrPathChanged", err)
	}
	if b, _ := os.ReadFile(env); string(b) != "SECRET" {
		t.Fatalf(".env was written: %q", b)
	}
}
