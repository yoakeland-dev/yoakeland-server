package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/fsutil"
)

// ARC-4: the helper replaces content, applies the requested mode exactly, and
// leaves no temp file behind, on success or on failure.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(path, []byte("new"), 0600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q, want new", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}

	// A missing directory fails the temp create and must not leave anything.
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "nope", "x.json"), []byte("x"), 0600); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files in %s: %v", dir, entries)
	}
}
