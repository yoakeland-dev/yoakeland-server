package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
	"yoakeland/internal/diff"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

const (
	// maxEditableSize caps read_file's content: JSON escaping can double a
	// file's size, and it all has to fit in one 10MB frame.
	maxEditableSize = 2 << 20
	maxDiffSize     = 2 << 20
	maxTreeEntries  = 5000
)

// fileWriteMu makes write_file's version check and write one step, so two
// editors saving at once can't both pass the check.
// ponytail: one lock for every project; per-project locks if saves ever queue.
var fileWriteMu sync.Mutex

// handleFileEnvelope serves the explorer, editor and diff requests (SPEC.md
// §3.1 "Project files"). It reports whether env was one of them.
func handleFileEnvelope(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) bool {
	reply := func(t protocol.EventType, payload any) {
		_ = ipc.SendEvent(t, env.SessionID, env.TurnID, "", payload)
	}
	switch env.Type {
	case protocol.EventListTree:
		var d protocol.FileRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.TreeListData{RequestID: d.RequestID, Path: d.Path, Entries: []protocol.TreeEntry{}}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.ProjectPath = pc.Root
			out.Entries, err = listTree(pc.Root, d.Path)
		}
		out.Error = errString(err)
		reply(protocol.EventTreeList, out)

	case protocol.EventReadFile:
		var d protocol.FileRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.FileContentData{RequestID: d.RequestID, Path: d.Path}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.ProjectPath = pc.Root
			err = readProjectFile(pc.Root, &out)
		}
		out.Error = errString(err)
		reply(protocol.EventFileContent, out)

	case protocol.EventWriteFile:
		var d protocol.WriteFileData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.FileWrittenData{RequestID: d.RequestID, Path: d.Path}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.ProjectPath = pc.Root
			out.Version, out.Conflict, err = writeProjectFile(pc.Root, d)
		}
		out.Error = errString(err)
		reply(protocol.EventFileWritten, out)
		if err == nil && !out.Conflict {
			filesChanged(deps, pc.Root, d.Path)
		}

	case protocol.EventFsOp:
		var d protocol.FsOpData
		_ = json.Unmarshal(env.Data, &d)
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			err = fsOp(pc.Root, d)
		}
		reply(protocol.EventFsOpDone, protocol.FsOpDoneData{RequestID: d.RequestID, Error: errString(err)})
		if err == nil {
			filesChanged(deps, pc.Root, d.Path, d.NewPath)
		}

	case protocol.EventListChanges:
		var d protocol.FileRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.GitChangesData{RequestID: d.RequestID, Files: []protocol.GitChange{}}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.ProjectPath = pc.Root
			out.Files, out.Truncated, err = gitChanges(pc.Root)
		}
		out.Error = errString(err)
		reply(protocol.EventGitChanges, out)

	case protocol.EventGetFileDiff:
		var d protocol.FileRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.FileDiffData{RequestID: d.RequestID, Path: d.Path}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.ProjectPath = pc.Root
			err = fileDiff(pc, d, &out)
		}
		out.Error = errString(err)
		reply(protocol.EventFileDiff, out)

	default:
		return false
	}
	return true
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fileProject(deps connDeps, projectPath string) (*ProjectContext, error) {
	if projectPath == "" {
		projectPath = deps.defaultProjectRoot
	}
	if projectPath == "" {
		return nil, errors.New("projectPath is required (no default project is configured)")
	}
	return deps.projects.Get(projectPath)
}

// filesChanged tells every peer, the sender included, which paths changed,
// and rechecks git since working-tree edits don't touch .git.
func filesChanged(deps connDeps, root string, paths ...string) {
	var out []string
	for _, p := range paths {
		if p != "" {
			out = append(out, path.Clean(p))
		}
	}
	deps.peers.BroadcastEvent("", protocol.EventFilesChanged, "", protocol.FilesChangedData{ProjectPath: root, Paths: out})
	deps.projects.git(root).Poke()
}

// resolveFile resolves a client's project-relative path, following symlinks
// but refusing anything that ends up outside the project.
func resolveFile(root, rel string) (string, error) {
	norm, err := normalizer.New(root)
	if err != nil {
		return "", err
	}
	return norm.ResolveAndValidatePath(filepath.FromSlash(rel))
}

// resolveEntry is resolveFile for an entry being created, renamed or
// deleted: its parent is resolved and checked, but the entry itself isn't
// followed, so deleting a symlink deletes the link and not its target.
func resolveEntry(root, rel string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(rel))
	if clean == "/" {
		return "", errors.New("that's the project root")
	}
	dir, err := resolveFile(root, path.Dir(clean[1:]))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, path.Base(clean)), nil
}

func listTree(root, rel string) ([]protocol.TreeEntry, error) {
	dir, err := resolveFile(root, rel)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.TreeEntry, 0, len(raw))
	for _, e := range raw {
		if e.Name() == ".git" {
			continue
		}
		te := protocol.TreeEntry{Name: e.Name(), IsDir: e.IsDir(), IsSymlink: e.Type()&os.ModeSymlink != 0}
		if te.IsSymlink {
			if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				te.IsDir = fi.IsDir()
			}
		}
		if !te.IsDir {
			if fi, err := e.Info(); err == nil {
				te.Size = fi.Size()
			}
		}
		out = append(out, te)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Name < out[j].Name
	})
	// ponytail: silently capped; add a truncated flag if huge folders matter.
	if len(out) > maxTreeEntries {
		out = out[:maxTreeEntries]
	}
	return out, nil
}

// fileVersion identifies content for write_file's conflict check.
func fileVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// isBinary treats a NUL byte near the start, or invalid UTF-8, as binary.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 || !utf8.Valid(data)
}

func readProjectFile(root string, out *protocol.FileContentData) error {
	p, err := resolveFile(root, out.Path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("is a folder")
	}
	out.Size = fi.Size()
	if fi.Size() > maxEditableSize {
		out.TooLarge = true
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out.Version = fileVersion(data)
	if isBinary(data) {
		out.Binary = true
		return nil
	}
	out.Content = string(data)
	return nil
}

func writeProjectFile(root string, d protocol.WriteFileData) (version string, conflict bool, err error) {
	p, err := resolveFile(root, d.Path)
	if err != nil {
		return "", false, err
	}
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()
	if !d.Force {
		current := ""
		if data, err := os.ReadFile(p); err == nil {
			current = fileVersion(data)
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
		if current != d.BaseVersion {
			return current, true, nil
		}
	}
	if err := tool.WriteFileAtomic(p, []byte(d.Content)); err != nil {
		return "", false, err
	}
	return fileVersion([]byte(d.Content)), false, nil
}

func fsOp(root string, d protocol.FsOpData) error {
	p, err := resolveEntry(root, d.Path)
	if err != nil {
		return err
	}
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return err == nil
	}
	switch d.Op {
	case "create":
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		return f.Close()
	case "mkdir":
		if exists(p) {
			return fmt.Errorf("%s already exists", d.Path)
		}
		return os.MkdirAll(p, 0755)
	case "rename":
		dst, err := resolveEntry(root, d.NewPath)
		if err != nil {
			return err
		}
		if exists(dst) {
			return fmt.Errorf("%s already exists", d.NewPath)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return os.Rename(p, dst)
	case "delete":
		if !exists(p) {
			return fmt.Errorf("%s doesn't exist", d.Path)
		}
		return os.RemoveAll(p)
	}
	return fmt.Errorf("unknown op %q", d.Op)
}

// fileDiff compares the working copy of d.Path with its HEAD copy (or
// d.BasePath's, for a rename), or with a snapshot's pre-turn copy.
func fileDiff(pc *ProjectContext, d protocol.FileRequestData, out *protocol.FileDiffData) error {
	p, err := resolveFile(pc.Root, d.Path)
	if err != nil {
		return err
	}
	after, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var before []byte
	switch {
	case d.SnapshotID != "":
		before, _, err = pc.SnapStore.ReadBackup(d.SnapshotID, d.Path)
	case d.BasePath != "":
		before, _, err = gitShowHead(pc.Root, d.BasePath)
	default:
		before, _, err = gitShowHead(pc.Root, d.Path)
	}
	if err != nil {
		return err
	}
	if len(before) > maxDiffSize || len(after) > maxDiffSize {
		return errors.New("too large to diff")
	}
	if isBinary(before) || isBinary(after) {
		out.Binary = true
		return nil
	}
	out.Diff = diff.ComputeDiff(d.Path, string(before), string(after))
	return nil
}
