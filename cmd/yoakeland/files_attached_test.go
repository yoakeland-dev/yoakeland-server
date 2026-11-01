package main

import (
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

func TestAttachedMedia(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, []byte("not really a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := tool.ResolveAttached(png, []string{png}); !ok {
		t.Fatal("an attached file should resolve")
	}
	if _, ok := tool.ResolveAttached(filepath.Join(dir, "other.png"), []string{png}); ok {
		t.Fatal("a file that was not attached must not resolve")
	}
	var out protocol.MediaInfoData
	if err := attachedMediaInfo(png, &out); err != nil || out.Mime != "image/png" || out.Size != 16 {
		t.Fatalf("info = %+v, %v", out, err)
	}
	data, last, err := readAttachedChunk(png, 0)
	if err != nil || !last || string(data) != "not really a png" {
		t.Fatalf("chunk = %q %v %v", data, last, err)
	}
}
