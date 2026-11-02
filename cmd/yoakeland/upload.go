package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/thumb"
)

const (
	// maxUploadSize bounds one upload. Big enough for logs, datasets and
	// screenshots; anything larger belongs in the repo or on a share.
	maxUploadSize = 256 << 20
	// uploadMaxAge is the default retention: an upload nobody has read,
	// re-uploaded or attached for this long is pruned, unless a stored chat
	// still references it (see prune). Runtime setting uploadRetentionDays
	// overrides it; 0 keeps everything forever.
	uploadMaxAge = 30 * 24 * time.Hour
	// touchInterval throttles directory mtime updates: a large video read in
	// 512 KB chunks would otherwise touch the same hash hundreds of times.
	touchInterval = time.Minute
	// thumbsDir holds generated previews, one <sha>.jpg per content hash.
	thumbsDir = ".thumbs"
	// Uploads in progress each hold an open file and may grow to maxUploadSize before their
	// checksum is known, so a peer can't start them without limit.
	maxPendingPerPeer = 4
	maxPendingTotal   = 32
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// sha256Anywhere finds a hash inside a session's JSON text (anchors can't).
	sha256Anywhere = regexp.MustCompile(`[0-9a-f]{64}`)
)

// UploadStore keeps client-uploaded attachment files under
// <-state-dir>/uploads/<sha256>/<name>: content-addressed, so the same bytes
// uploaded twice (or from two devices) are stored once and the second
// upload_begin finishes without sending anything. Server-wide rather than
// per-project so one file attached in several projects isn't duplicated.
// Generated previews live under uploads/.thumbs, keyed by the same hash.
// A nil *UploadStore rejects every upload (tests' bare connDeps).
type UploadStore struct {
	dir      string
	stateDir string

	mu        sync.Mutex
	pending   map[string]*pendingUpload // peerID + "/" + uploadID
	retention time.Duration             // 0 keeps everything
	touched   map[string]time.Time      // sha -> last mtime bump, throttles Touch
}

type pendingUpload struct {
	f        *os.File
	h        hash.Hash
	name     string
	sha      string
	size     int64
	received int64
}

// NewUploadStore creates the store and prunes it once. The optional retention
// (0 = keep forever) is the runtime setting; without one it defaults to
// uploadMaxAge.
func NewUploadStore(stateDir string, retention ...time.Duration) (*UploadStore, error) {
	dir := filepath.Join(stateDir, "uploads")
	// Any .tmp left behind belongs to a connection from a previous process.
	_ = os.RemoveAll(filepath.Join(dir, ".tmp"))
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, thumbsDir), 0o700); err != nil {
		return nil, err
	}
	s := &UploadStore{
		dir:       dir,
		stateDir:  stateDir,
		pending:   make(map[string]*pendingUpload),
		retention: uploadMaxAge,
		touched:   make(map[string]time.Time),
	}
	if len(retention) > 0 {
		s.retention = retention[0]
	}
	s.Prune()
	return s, nil
}

// uploadRetention turns the runtime setting into a duration; 0 means forever.
func uploadRetention(rs protocol.RuntimeSettings) time.Duration {
	if rs.UploadRetentionDays <= 0 {
		return 0
	}
	return time.Duration(rs.UploadRetentionDays) * 24 * time.Hour
}

// SetRetention changes how long an unreferenced upload may sit unread. Zero
// disables pruning entirely. It takes effect on the next Prune.
func (s *UploadStore) SetRetention(d time.Duration) {
	if s == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	s.mu.Lock()
	s.retention = d
	s.mu.Unlock()
}

// PrunePeriodically runs Prune in the background until the process exits.
// Hourly checks catch a server that runs for months without a restart.
func (s *UploadStore) PrunePeriodically(every time.Duration) {
	if s == nil || every <= 0 {
		return
	}
	for range time.Tick(every) {
		s.Prune()
	}
}

// Prune deletes unreferenced uploads older than the retention. A chat that
// still lists the path keeps the content alive regardless of age; deleting
// that chat makes it eligible again. Retention 0 means never prune.
func (s *UploadStore) Prune() {
	if s == nil {
		return
	}
	s.mu.Lock()
	maxAge := s.retention
	s.mu.Unlock()
	if maxAge <= 0 {
		return
	}
	referenced := s.referenced()
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if !sha256Hex.MatchString(e.Name()) || referenced[e.Name()] {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(s.dir, e.Name()))
			_ = os.Remove(filepath.Join(s.dir, thumbsDir, e.Name()+".jpg"))
		}
	}
	// A thumbnail whose original is already gone serves nothing.
	thumbs, _ := os.ReadDir(filepath.Join(s.dir, thumbsDir))
	for _, t := range thumbs {
		sha := strings.TrimSuffix(t.Name(), ".jpg")
		if !sha256Hex.MatchString(sha) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, sha)); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(filepath.Join(s.dir, thumbsDir, t.Name()))
		}
	}
}

// referenced is every content hash a stored session still names. Sessions
// keep attachment absolute paths, so their JSON text carries the hash.
// Scanning is bounded to <-state-dir>/projects (project and chat sessions
// both live beneath it) and happens only during a prune.
func (s *UploadStore) referenced() map[string]bool {
	out := make(map[string]bool)
	if s.stateDir == "" {
		return out
	}
	_ = filepath.WalkDir(filepath.Join(s.stateDir, "projects"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, m := range sha256Anywhere.FindAll(b, -1) {
			out[string(m)] = true
		}
		return nil
	})
	return out
}

// shaOf reports the content hash of a path inside an upload's directory.
func (s *UploadStore) shaOf(abs string) (string, bool) {
	if s == nil {
		return "", false
	}
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || !sha256Hex.MatchString(parts[0]) {
		return "", false
	}
	return parts[0], true
}

// Touch marks abs as recently used, so age-based pruning leaves it alone even
// when the client only reads it. Paths outside the store are ignored.
func (s *UploadStore) Touch(abs string) {
	sha, ok := s.shaOf(abs)
	if !ok {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.touched[sha]; ok && now.Sub(last) < touchInterval {
		s.mu.Unlock()
		return
	}
	s.touched[sha] = now
	s.mu.Unlock()
	_ = os.Chtimes(filepath.Join(s.dir, sha), now, now)
}

// Thumb returns a cached preview of the image at abs, generating and storing
// one on first use. ok is false for anything it cannot decode (a video, a
// WebP, a path outside the store), and the caller serves the original.
func (s *UploadStore) Thumb(abs string) (path string, ok bool) {
	sha, inStore := s.shaOf(abs)
	if !inStore {
		return "", false
	}
	dst := filepath.Join(s.dir, thumbsDir, sha+".jpg")
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		s.Touch(abs)
		return dst, true
	}
	data, err := thumb.Generate(abs, thumb.MaxDim)
	if err != nil {
		return "", false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", false
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "th-*")
	if err != nil {
		return "", false
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return "", false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", false
	}
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return "", false
	}
	s.Touch(abs)
	return dst, true
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
