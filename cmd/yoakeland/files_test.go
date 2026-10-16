package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
)

// One pass over the explorer/editor requests: escape refusal, the write
// conflict check, and that deleting a symlink removes the link, not its
// target outside the project.
func TestFileRequests(t *testing.T) {
	deps := newTestDeps(t)
	root := deps.defaultProjectRoot
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	one := func(typ protocol.EventType, data any, out any) {
		// Fresh values each time: omitempty fields wouldn't overwrite old ones.
		switch o := out.(type) {
		case *protocol.FileContentData:
			*o = protocol.FileContentData{}
		case *protocol.FileWrittenData:
			*o = protocol.FileWrittenData{}
		case *protocol.FsOpDoneData:
			*o = protocol.FsOpDoneData{}
		}
		t.Helper()
		envs := runDispatch(t, deps, envelopeJSON(t, typ, data))
		if len(envs) == 0 {
			t.Fatalf("%s: no reply", typ)
		}
		if err := json.Unmarshal(envs[0].Data, out); err != nil {
			t.Fatal(err)
		}
	}

	var read protocol.FileContentData
	one(protocol.EventReadFile, protocol.FileRequestData{Path: "../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt"}, &read)
	if read.Error == "" || read.Content != "" {
		t.Fatalf("read outside the project should fail, got %+v", read)
	}
	one(protocol.EventReadFile, protocol.FileRequestData{Path: "link"}, &read)
	if read.Error == "" {
		t.Fatalf("reading through a symlink that leaves the project should fail, got %+v", read)
	}

	var w protocol.FileWrittenData
	one(protocol.EventWriteFile, protocol.WriteFileData{Path: "src/a.txt", Content: "v1"}, &w)
	if w.Error != "" || w.Conflict || w.Version == "" {
		t.Fatalf("creating a file: %+v", w)
	}
	v1 := w.Version
	one(protocol.EventWriteFile, protocol.WriteFileData{Path: "src/a.txt", Content: "v2", BaseVersion: "stale"}, &w)
	if !w.Conflict || w.Version != v1 {
		t.Fatalf("a stale baseVersion should conflict and report the current one, got %+v", w)
	}
	one(protocol.EventWriteFile, protocol.WriteFileData{Path: "src/a.txt", Content: "v2", BaseVersion: v1}, &w)
	if w.Error != "" || w.Conflict {
		t.Fatalf("saving over the read version: %+v", w)
	}

	var tree protocol.TreeListData
	one(protocol.EventListTree, protocol.FileRequestData{Path: ""}, &tree)
	if len(tree.Entries) != 2 || tree.Entries[0].Name != "src" || !tree.Entries[0].IsDir || !tree.Entries[1].IsSymlink {
		t.Fatalf("unexpected root listing: %+v", tree.Entries)
	}

	var done protocol.FsOpDoneData
	one(protocol.EventFsOp, protocol.FsOpData{Op: "rename", Path: "src/a.txt", NewPath: "b.txt"}, &done)
	if done.Error != "" {
		t.Fatal(done.Error)
	}
	one(protocol.EventFsOp, protocol.FsOpData{Op: "delete", Path: "link"}, &done)
	if done.Error != "" {
		t.Fatal(done.Error)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("deleting the link touched its target: %q %v", data, err)
	}
	one(protocol.EventFsOp, protocol.FsOpData{Op: "delete", Path: ""}, &done)
	if done.Error == "" {
		t.Fatal("deleting the project root should be refused")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "b.txt")); string(data) != "v2" {
		t.Fatalf("rename lost content: %q", data)
	}
}

// fs_op resolves only the entry's parent, so the Hidden state dir itself
// (a project opened at $HOME contains ~/.yoake) must be refused by name.
func TestFsOpRefusesStateDir(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, ".yoake")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	old := normalizer.Hidden
	normalizer.Hidden = []string{state}
	t.Cleanup(func() { normalizer.Hidden = old })

	for _, d := range []protocol.FsOpData{
		{Op: "delete", Path: ".yoake"},
		{Op: "delete", Path: "./x/../.yoake/"},
		{Op: "rename", Path: ".yoake", NewPath: "gone"},
	} {
		if err := fsOp(root, d); err == nil {
			t.Fatalf("%+v should be refused", d)
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state dir touched: %v", err)
	}
}

// A git dir inside a submodule is refused by every explorer write, though
// reads of it stay open.
func TestWriteProjectFileRefusesSubmoduleGit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "modules", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := writeProjectFile(root, protocol.WriteFileData{Path: ".git/modules/sub/config", Content: "[filter \"x\"]", Force: true})
	if !errors.Is(err, normalizer.ErrGitExecPath) {
		t.Fatalf("err = %v, want ErrGitExecPath", err)
	}
}
