package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// read_media size caps: images and fonts stay small enough that the
	// client's object-URL blob and the browser's decoder both stay
	// comfortable; video, audio and PDFs get much more room since they
	// legitimately run into the tens of MB.
	maxMediaImageSize = 25 << 20
	maxMediaLargeSize = 200 << 20
	// mediaChunkSize is what one read_media_chunk reply carries, decoded —
	// same "keep it well under the 10MB frame limit" convention as uploads.
	mediaChunkSize = 512 * 1024
)

// mediaMimeByExt recognizes the types the explorer can actually render
// (<img>, <video>, <audio>, a PDF frame, a FontFace), by extension — cheaper and more portable
// across platforms than sniffing content or trusting the OS mime database.
var mediaMimeByExt = map[string]string{
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".bmp":   "image/bmp",
	".ico":   "image/x-icon",
	".svg":   "image/svg+xml",
	".avif":  "image/avif",
	".apng":  "image/apng",
	".jfif":  "image/jpeg",
	".cur":   "image/x-icon",
	".mp4":   "video/mp4",
	".m4v":   "video/mp4",
	".webm":  "video/webm",
	".ogv":   "video/ogg",
	".mov":   "video/quicktime",
	".mp3":   "audio/mpeg",
	".wav":   "audio/wav",
	".ogg":   "audio/ogg",
	".oga":   "audio/ogg",
	".opus":  "audio/ogg",
	".flac":  "audio/flac",
	".m4a":   "audio/mp4",
	".aac":   "audio/aac",
	".pdf":   "application/pdf",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// mediaMime also says whether the type gets the larger size cap.
func mediaMime(name string) (mime string, large bool) {
	mime = mediaMimeByExt[strings.ToLower(filepath.Ext(name))]
	large = strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") || mime == "application/pdf"
	return mime, large
}

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

	case protocol.EventReadMedia:
		var d protocol.FileRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.MediaInfoData{RequestID: d.RequestID, Path: d.Path}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			err = readMediaInfo(pc.Root, d.Path, &out)
		}
		out.Error = errString(err)
		reply(protocol.EventMediaInfo, out)

	case protocol.EventReadMediaChunk:
		var d protocol.MediaChunkRequestData
		_ = json.Unmarshal(env.Data, &d)
		out := protocol.MediaChunkData{RequestID: d.RequestID, Offset: d.Offset}
		pc, err := fileProject(deps, d.ProjectPath)
		if err == nil {
			out.Data, out.Last, err = readMediaChunk(pc.Root, d.Path, d.Offset)
		}
		out.Error = errString(err)
		reply(protocol.EventMediaChunk, out)

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

// refuseGitExec guards every write path the explorer offers: a remote peer
// may only hold a project, and .git/config or a hook written here runs as
// code the next time git runs on the host (see hostGit).
func refuseGitExec(root string, paths ...string) error {
	norm, err := normalizer.New(root)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if norm.IsGitExecPath(p) {
			return normalizer.ErrGitExecPath
		}
	}
	return nil
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
	var out []protocol.TreeEntry
	err = normalizer.InRoot(root, dir, func(r *os.Root, rel string) error {
		f, err := r.Open(rel)
		if err != nil {
			return err
		}
		raw, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		out = make([]protocol.TreeEntry, 0, len(raw))
		for _, e := range raw {
			if e.Name() == ".git" {
				continue
			}
			te := protocol.TreeEntry{Name: e.Name(), IsDir: e.IsDir(), IsSymlink: e.Type()&os.ModeSymlink != 0}
			if te.IsSymlink {
				if fi, err := r.Stat(filepath.Join(rel, e.Name())); err == nil {
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
		return nil
	})
	if err != nil {
		return nil, err
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
	var data []byte
	err = normalizer.InRoot(root, p, func(r *os.Root, rel string) error {
		fi, err := r.Stat(rel)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return errors.New("is a folder or a special file")
		}
		out.Size = fi.Size()
		if fi.Size() > maxEditableSize {
			out.TooLarge = true
			return nil
		}
		data, err = r.ReadFile(rel)
		return err
	})
	if err != nil || out.TooLarge {
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

// readMediaInfo answers read_media: the size and mime type a recognized
// file will preview at, or an error (unrecognized extension, too
// large, not found) with no further read_media_chunk expected to follow.
func readMediaInfo(root, rel string, out *protocol.MediaInfoData) error {
	mime, large := mediaMime(rel)
	if mime == "" {
		return errors.New("not a previewable type")
	}
	p, err := resolveFile(root, rel)
	if err != nil {
		return err
	}
	var fi os.FileInfo
	if err := normalizer.InRoot(root, p, func(r *os.Root, rel string) (err error) {
		fi, err = r.Stat(rel)
		return err
	}); err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("is a folder or a special file")
	}
	limit := int64(maxMediaImageSize)
	if large {
		limit = maxMediaLargeSize
	}
	if fi.Size() > limit {
		return fmt.Errorf("too large to preview (%d MB, limit %d MB)", fi.Size()>>20, limit>>20)
	}
	out.Size = fi.Size()
	out.Mime = mime
	return nil
}

// readMediaChunk reads one mediaChunkSize slice starting at offset, without
// holding the whole file in memory — the point of read_media_chunk over
// just inlining content on media_info for anything video-sized.
func readMediaChunk(root, rel string, offset int64) (data []byte, last bool, err error) {
	p, err := resolveFile(root, rel)
	if err != nil {
		return nil, false, err
	}
	var f *os.File
	if err := normalizer.InRoot(root, p, func(r *os.Root, rel string) (err error) {
		f, err = r.Open(rel)
		return err
	}); err != nil {
		return nil, false, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	buf := make([]byte, mediaChunkSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, false, err
	}
	buf = buf[:n]
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	return buf, offset+int64(n) >= fi.Size(), nil
}

func writeProjectFile(root string, d protocol.WriteFileData) (version string, conflict bool, err error) {
	p, err := resolveFile(root, d.Path)
	if err != nil {
		return "", false, err
	}
	if err := refuseGitExec(root, p); err != nil {
		return "", false, err
	}
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()
	if !d.Force {
		current := ""
		err := normalizer.InRoot(root, p, func(r *os.Root, rel string) error {
			data, err := r.ReadFile(rel)
			if err == nil {
				current = fileVersion(data)
			}
			return err
		})
		if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
		if current != d.BaseVersion {
			return current, true, nil
		}
	}
	if err := tool.WriteFileAtomic(root, p, []byte(d.Content)); err != nil {
		return "", false, err
	}
	return fileVersion([]byte(d.Content)), false, nil
}

func fsOp(root string, d protocol.FsOpData) error {
	p, err := resolveEntry(root, d.Path)
	if err != nil {
		return err
	}
	if err := refuseGitExec(root, p); err != nil {
		return err
	}
	var dst string
	if d.Op == "rename" {
		if dst, err = resolveEntry(root, d.NewPath); err != nil {
			return err
		}
		if err := refuseGitExec(root, dst); err != nil {
			return err
		}
	}
	// Through an os.Root: a directory swapped for a symlink after
	// resolveEntry checked it can't send the operation out of the project.
	return normalizer.InRoot(root, p, func(r *os.Root, rel string) error {
		exists := func(rel string) bool {
			_, err := r.Lstat(rel)
			return err == nil
		}
		switch d.Op {
		case "create":
			if err := r.MkdirAll(filepath.Dir(rel), 0755); err != nil {
				return err
			}
			f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err != nil {
				return err
			}
			return f.Close()
		case "mkdir":
			if exists(rel) {
				return fmt.Errorf("%s already exists", d.Path)
			}
			return r.MkdirAll(rel, 0755)
		case "rename":
			dstRel, err := filepath.Rel(root, dst)
			if err != nil {
				return err
			}
			if exists(dstRel) {
				return fmt.Errorf("%s already exists", d.NewPath)
			}
			if err := r.MkdirAll(filepath.Dir(dstRel), 0755); err != nil {
				return err
			}
			return r.Rename(rel, dstRel)
		case "delete":
			if !exists(rel) {
				return fmt.Errorf("%s doesn't exist", d.Path)
			}
			return r.RemoveAll(rel)
		}
		return fmt.Errorf("unknown op %q", d.Op)
	})
}

// fileDiff compares the working copy of d.Path with its HEAD copy (or
// d.BasePath's, for a rename), or with a snapshot's pre-turn copy.
func fileDiff(pc *ProjectContext, d protocol.FileRequestData, out *protocol.FileDiffData) error {
	p, err := resolveFile(pc.Root, d.Path)
	if err != nil {
		return err
	}
	var after []byte
	err = normalizer.InRoot(pc.Root, p, func(r *os.Root, rel string) (err error) {
		after, err = r.ReadFile(rel)
		return err
	})
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
