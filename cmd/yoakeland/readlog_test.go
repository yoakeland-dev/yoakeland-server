package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	os.WriteFile(p, []byte("aaa\nbbb\nccc\n"), 0600)
	if s, tr, _ := tailFile(p, 100); s != "aaa\nbbb\nccc\n" || tr {
		t.Fatalf("whole: %q %v", s, tr)
	}
	if s, tr, _ := tailFile(p, 6); s != "ccc\n" || !tr {
		t.Fatalf("tail: %q %v", s, tr)
	}
}
