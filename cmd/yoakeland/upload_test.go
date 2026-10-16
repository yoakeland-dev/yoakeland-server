package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
)

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestUploadStore_ChunkedDedupAndMismatch(t *testing.T) {
	s, err := NewUploadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello, uploaded world")
	begin := protocol.UploadBeginData{UploadID: "u1", Name: "../../evil/notes.txt", Size: int64(len(content)), SHA256: shaHex(content)}

	if _, done, err := s.Begin("p1", begin); err != nil || done {
		t.Fatalf("fresh begin: done=%v err=%v", done, err)
	}
	if path, got, err := s.Chunk("p1", protocol.UploadChunkData{UploadID: "u1", Data: content[:5]}); err != nil || path != "" || got != 5 {
		t.Fatalf("first chunk: %q %d %v", path, got, err)
	}
	path, _, err := s.Chunk("p1", protocol.UploadChunkData{UploadID: "u1", Data: content[5:]})
	if err != nil || path == "" {
		t.Fatalf("last chunk: %q %v", path, err)
	}
	if filepath.Base(path) != "notes.txt" || filepath.Dir(filepath.Dir(path)) != s.dir {
		t.Fatalf("name not sanitized into the store: %s", path)
	}
	if b, _ := os.ReadFile(path); string(b) != string(content) {
		t.Fatalf("stored content %q", b)
	}

	// Same bytes from another peer: finished without sending anything.
	begin.UploadID = "u2"
	if again, done, err := s.Begin("p2", begin); err != nil || !done || again != path {
		t.Fatalf("dedup: %q done=%v err=%v", again, done, err)
	}

	// Wrong content for the promised hash is rejected and not stored.
	bad := protocol.UploadBeginData{UploadID: "u3", Name: "x", Size: 3, SHA256: shaHex([]byte("abc"))}
	_, _, _ = s.Begin("p1", bad)
	if _, _, err := s.Chunk("p1", protocol.UploadChunkData{UploadID: "u3", Data: []byte("xyz")}); err == nil {
		t.Fatal("sha mismatch accepted")
	}
	if _, ok := s.existing(bad.SHA256); ok {
		t.Fatal("corrupted upload was stored")
	}
}

func TestValidateAttachments_TCPConfinedUnixNot(t *testing.T) {
	s, _ := NewUploadStore(t.TempDir())
	project := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("s"), 0o600)
	inProject := filepath.Join(project, "a.txt")
	_ = os.WriteFile(inProject, []byte("a"), 0o600)
	link := filepath.Join(project, "link")
	_ = os.Symlink(outside, link)

	if err := validateAttachments("unix", s, project, []string{outside}); err != nil {
		t.Fatalf("unix may attach anything: %v", err)
	}
	if err := validateAttachments("tcp", s, project, []string{inProject}); err != nil {
		t.Fatalf("tcp in-project: %v", err)
	}
	for _, p := range []string{outside, link, "relative.txt"} {
		if err := validateAttachments("tcp", s, project, []string{p}); err == nil {
			t.Fatalf("tcp attached %s", p)
		}
	}
}

// A TCP peer can't attach yoakeland's own state (the tokens live there), but
// it can attach what it uploaded, which is stored under that state dir.
func TestValidateAttachments_RefusesStateDir(t *testing.T) {
	project, _ := normalizer.CanonicalRoot(t.TempDir())
	state := filepath.Join(project, ".yoake")
	s, err := NewUploadStore(state)
	if err != nil {
		t.Fatal(err)
	}
	oldHidden, oldUploads := normalizer.Hidden, normalizer.Uploads
	normalizer.Hidden, normalizer.Uploads = []string{state}, s.dir
	t.Cleanup(func() { normalizer.Hidden, normalizer.Uploads = oldHidden, oldUploads })

	token := filepath.Join(state, "server.json")
	_ = os.WriteFile(token, []byte("t"), 0o600)
	if err := validateAttachments("tcp", s, project, []string{token}); err == nil {
		t.Fatal("tcp attached the state dir")
	}
	up := filepath.Join(s.dir, "abc", "a.txt")
	_ = os.MkdirAll(filepath.Dir(up), 0o700)
	_ = os.WriteFile(up, []byte("a"), 0o600)
	if err := validateAttachments("tcp", s, project, []string{up}); err != nil {
		t.Fatalf("tcp upload: %v", err)
	}
}

// A peer can hold only maxPendingPerPeer uploads open at once, and a refused begin leaves no file behind.
func TestUploadStore_PendingLimit(t *testing.T) {
	s, err := NewUploadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPendingPerPeer; i++ {
		if _, _, err := s.Begin("p1", protocol.UploadBeginData{UploadID: string(rune('a' + i)), Name: "f", Size: 10, SHA256: shaHex([]byte("0123456789"))}); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	if _, _, err := s.Begin("p1", protocol.UploadBeginData{UploadID: "over", Name: "f", Size: 10, SHA256: shaHex([]byte("0123456789"))}); err == nil {
		t.Fatal("peer exceeded its pending uploads")
	}
	if _, _, err := s.Begin("p2", protocol.UploadBeginData{UploadID: "other", Name: "f", Size: 10, SHA256: shaHex([]byte("0123456789"))}); err != nil {
		t.Fatalf("another peer was refused: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(s.dir, ".tmp")); len(left) != maxPendingPerPeer+1 {
		t.Fatalf("%d temp files, want %d (a refused begin must not leave one)", len(left), maxPendingPerPeer+1)
	}
}
