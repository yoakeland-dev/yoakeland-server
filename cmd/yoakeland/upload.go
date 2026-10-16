package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
)

const (
	// maxUploadSize bounds one upload. Big enough for logs, datasets and
	// screenshots; anything larger belongs in the repo or on a share.
	maxUploadSize = 256 << 20
	// uploadMaxAge: an upload untouched (neither uploaded nor re-used) this
	// long is pruned at startup.
	// ponytail: age-based GC, a session referencing an older upload just
	// can't read it anymore; switch to reference counting if that bites.
	uploadMaxAge = 30 * 24 * time.Hour
	// Uploads in progress each hold an open file and may grow to maxUploadSize before their
	// checksum is known, so a peer can't start them without limit.
	maxPendingPerPeer = 4
	maxPendingTotal   = 32
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// UploadStore keeps client-uploaded attachment files under
// <-state-dir>/uploads/<sha256>/<name>: content-addressed, so the same bytes
// uploaded twice (or from two devices) are stored once and the second
// upload_begin finishes without sending anything. Server-wide rather than
// per-project so one file attached in several projects isn't duplicated.
// A nil *UploadStore rejects every upload (tests' bare connDeps).
type UploadStore struct {
	dir     string
	mu      sync.Mutex
	pending map[string]*pendingUpload // peerID + "/" + uploadID
}

type pendingUpload struct {
	f        *os.File
	h        hash.Hash
	name     string
	sha      string
	size     int64
	received int64
}

func NewUploadStore(stateDir string) (*UploadStore, error) {
	dir := filepath.Join(stateDir, "uploads")
	// Any .tmp left behind belongs to a connection from a previous process.
	_ = os.RemoveAll(filepath.Join(dir, ".tmp"))
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o700); err != nil {
		return nil, err
	}
	s := &UploadStore{dir: dir, pending: make(map[string]*pendingUpload)}
	s.prune(uploadMaxAge)
	return s, nil
}

func (s *UploadStore) prune(maxAge time.Duration) {
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if !sha256Hex.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(s.dir, e.Name()))
		}
	}
}

// existing returns the stored file for sha, if any, and marks it as used so
// prune keeps it around.
func (s *UploadStore) existing(sha string) (string, bool) {
	dir := filepath.Join(s.dir, sha)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return "", false
	}
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	return filepath.Join(dir, entries[0].Name()), true
}

func safeUploadName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." || name == "" {
		return "file"
	}
	return name
}

// Begin starts an upload. done is true (with path set) when the content is
// already stored and no chunks need to be sent.
func (s *UploadStore) Begin(peerID string, d protocol.UploadBeginData) (path string, done bool, err error) {
	if s == nil {
		return "", false, errors.New("uploads are not available on this server")
	}
	sha := strings.ToLower(d.SHA256)
	if d.UploadID == "" || !sha256Hex.MatchString(sha) {
		return "", false, errors.New("upload_begin needs an uploadId and a hex sha256")
	}
	if d.Size < 0 || d.Size > maxUploadSize {
		return "", false, fmt.Errorf("file is larger than the %d MB upload limit", maxUploadSize>>20)
	}
	if p, ok := s.existing(sha); ok {
		return p, true, nil
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, ".tmp"), "up-*")
	if err != nil {
		return "", false, err
	}
	key := peerID + "/" + d.UploadID
	s.mu.Lock()
	if old := s.pending[key]; old != nil {
		old.discard()
		delete(s.pending, key)
	}
	perPeer := 0
	for k := range s.pending {
		if strings.HasPrefix(k, peerID+"/") {
			perPeer++
		}
	}
	if perPeer >= maxPendingPerPeer || len(s.pending) >= maxPendingTotal {
		s.mu.Unlock()
		f.Close()
		_ = os.Remove(f.Name())
		return "", false, errors.New("too many uploads in progress; finish one first")
	}
	s.pending[key] = &pendingUpload{f: f, h: sha256.New(), name: safeUploadName(d.Name), sha: sha, size: d.Size}
	s.mu.Unlock()
	if d.Size == 0 {
		p, _, err := s.Chunk(peerID, protocol.UploadChunkData{UploadID: d.UploadID})
		return p, err == nil, err
	}
	return "", false, nil
}

// Chunk appends the next slice. When the last byte arrives the content is
// verified against the promised sha256 and moved into place; path is set
// then. Any error aborts the upload.
func (s *UploadStore) Chunk(peerID string, d protocol.UploadChunkData) (path string, received int64, err error) {
	if s == nil {
		return "", 0, errors.New("uploads are not available on this server")
	}
	key := peerID + "/" + d.UploadID
	s.mu.Lock()
	u := s.pending[key]
	s.mu.Unlock()
	if u == nil {
		return "", 0, errors.New("unknown uploadId")
	}
	fail := func(err error) (string, int64, error) {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		u.discard()
		return "", u.received, err
	}
	if u.received+int64(len(d.Data)) > u.size {
		return fail(errors.New("more data than the declared size"))
	}
	if _, err := u.f.Write(d.Data); err != nil {
		return fail(err)
	}
	u.h.Write(d.Data)
	u.received += int64(len(d.Data))
	if u.received < u.size {
		return "", u.received, nil
	}

	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()
	if got := hex.EncodeToString(u.h.Sum(nil)); got != u.sha {
		u.discard()
		return "", u.received, errors.New("sha256 mismatch, upload corrupted")
	}
	tmp := u.f.Name()
	if err := u.f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", u.received, err
	}
	// A concurrent upload of the same content may have won the race.
	if p, ok := s.existing(u.sha); ok {
		_ = os.Remove(tmp)
		return p, u.received, nil
	}
	dir := filepath.Join(s.dir, u.sha)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = os.Remove(tmp)
		return "", u.received, err
	}
	final := filepath.Join(dir, u.name)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", u.received, err
	}
	return final, u.received, nil
}

// DropPeer abandons every unfinished upload of a disconnected peer.
func (s *UploadStore) DropPeer(peerID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, u := range s.pending {
		if strings.HasPrefix(key, peerID+"/") {
			u.discard()
			delete(s.pending, key)
		}
	}
}

func (u *pendingUpload) discard() {
	_ = u.f.Close()
	_ = os.Remove(u.f.Name())
}

// validateAttachments enforces who may attach what (SPEC.md §3.1
// user_prompt attachments). The unix socket's user already owns every file
// this process can read, so anything absolute goes. A TCP peer holds only a
// token that grants the project, so its attachments must resolve (symlinks
// followed) inside the upload store or the project root — otherwise an
// attachment would let it read arbitrary files off the server.
func validateAttachments(network string, uploads *UploadStore, projectRoot string, paths []string) error {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("attachment %q must be an absolute path", p)
		}
		if network != "tcp" {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", p, err)
		}
		if normalizer.InHidden(real) {
			return fmt.Errorf("attachment %q is yoakeland's own state", p)
		}
		allowed := false
		for _, root := range []string{projectRoot, uploadsDir(uploads)} {
			if root == "" {
				continue
			}
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				continue
			}
			if rel, err := filepath.Rel(realRoot, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("attachment %q is outside the project and the upload store; upload it instead", p)
		}
	}
	return nil
}

func uploadsDir(s *UploadStore) string {
	if s == nil {
		return ""
	}
	return s.dir
}
