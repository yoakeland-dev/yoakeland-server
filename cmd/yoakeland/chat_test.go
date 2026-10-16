package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"yoakeland/internal/protocol"
)

func TestChatProject(t *testing.T) {
	r, err := NewProjectRegistry(nil, "", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := r.Resolve("c1", protocol.ChatProject, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.isChat(pc.Root) || !r.Known(pc.Root) {
		t.Fatalf("%q should be a known chat root", pc.Root)
	}
	if again, _ := r.Resolve("c1", "", ""); again != pc {
		t.Fatal("chat binding not kept")
	}
	if len(r.List()) != 0 {
		t.Fatal("a chat must not be listed as a project")
	}
	if _, err := r.Resolve("../x", protocol.ChatProject, ""); err == nil {
		t.Fatal("path-like session id accepted")
	}
	r.DropChat(pc.Root)
	if _, err := os.Stat(pc.Root); !os.IsNotExist(err) {
		t.Fatal("scratch root not removed")
	}
}

func TestChatRegistryIsChat(t *testing.T) {
	r, _ := NewProjectRegistry(nil, "", 0, t.TempDir(), "")
	pc, err := r.Resolve("c2", protocol.ChatProject, "")
	if err != nil {
		t.Fatal(err)
	}
	if !pc.Registry.IsChat() {
		t.Fatal("chat registry not marked chat")
	}
}

func TestChatWorkspaceAndPrune(t *testing.T) {
	state := t.TempDir()
	r, err := NewProjectRegistry(nil, "", 0, state, "")
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := r.chatRoot("stale")
	fresh, _ := r.chatRoot("fresh")
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	if err := os.WriteFile(filepath.Join(fresh, "a.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dirSize(fresh); got != 5 {
		t.Fatalf("dirSize = %d, want 5", got)
	}
	pruneChats(state, r.chatsDir)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("untouched, never-prompted chat dir not pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a recent chat dir was pruned")
	}
}

func TestImportChatFiles(t *testing.T) {
	chat, proj := t.TempDir(), t.TempDir()
	w := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(chat, p)), 0o755)
		if err := os.WriteFile(filepath.Join(chat, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("a.txt", "hi")
	w("sub/b.txt", "yo")
	w(".git/hooks/pre-commit", "boom")
	_ = os.Symlink("/etc/passwd", filepath.Join(chat, "link"))
	dir, files, _, err := importChatFiles(chat, proj, "c9")
	if err != nil || dir != "chat-c9" || files != 2 {
		t.Fatalf("dir=%q files=%d err=%v", dir, files, err)
	}
	for _, gone := range []string{".git", "link"} {
		if _, err := os.Lstat(filepath.Join(proj, dir, gone)); err == nil {
			t.Fatalf("%s was copied", gone)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(proj, dir, "sub/b.txt")); string(b) != "yo" {
		t.Fatal("nested file not copied")
	}
	if dir2, _, _, _ := importChatFiles(chat, proj, "c9"); dir2 != "chat-c9-2" {
		t.Fatalf("second import went to %q", dir2)
	}
	if _, _, _, err := importChatFiles(t.TempDir(), proj, "c10"); err == nil {
		t.Fatal("an empty workspace should be refused")
	}
}

// copyFile never follows a symlink out of the chat, even one that replaced a
// file the walk had listed as regular.
func TestCopyFileRefusesEscapingSymlink(t *testing.T) {
	chat, dst := t.TempDir(), t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("S"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(chat, "f")); err != nil {
		t.Fatal(err)
	}
	from, _ := os.OpenRoot(chat)
	defer from.Close()
	to, _ := os.OpenRoot(dst)
	defer to.Close()
	if err := copyFile(from, "f", to, "f", 0o644); err == nil {
		t.Fatal("copied through a symlink out of the chat")
	}
	if _, err := os.Lstat(filepath.Join(dst, "f")); err == nil {
		t.Fatal("a file was created for the refused copy")
	}
}
