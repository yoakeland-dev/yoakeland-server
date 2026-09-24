package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/agent"
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

func TestNotificationFeed_SessionTap(t *testing.T) {
	feed := NewNotificationFeed(NewPeerRegistry())
	store, err := agent.NewDiskSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tap := feed.sessionTap("/proj", store)
	tap(protocol.EventModelDelta, "s1", "t1", protocol.ModelDeltaData{})
	tap(protocol.EventTurnCompleted, "s1", "t1", protocol.TurnCompletedData{FinishReason: "cancelled"})
	tap(protocol.EventTurnCompleted, "s1", "t1", protocol.TurnCompletedData{FinishReason: "stop"})
	tap(protocol.EventPermissionRequest, "s1", "t1", protocol.PermissionRequestData{ToolName: "bash"})

	got := feed.List()
	if len(got) != 2 || got[0].Level != "success" || got[1].Title != "Permission needed: bash" || got[1].ProjectPath != "/proj" {
		t.Fatalf("unexpected notifications: %+v", got)
	}
	feed.Dismiss(got[0].ID)
	if l := feed.List(); len(l) != 1 || l[0].ID != got[1].ID {
		t.Fatalf("dismiss one: %+v", l)
	}
	feed.Dismiss("")
	if len(feed.List()) != 0 {
		t.Fatal("dismiss all")
	}
}
