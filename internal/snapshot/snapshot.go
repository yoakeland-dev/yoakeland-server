package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"yoakeland/internal/normalizer"
)

// ErrInvalidSnapshotID is returned when a snapshot id is not a plain basename
// (no path separators, no "..") and therefore cannot be safely joined onto
// the snapshot store's base directory.
var ErrInvalidSnapshotID = errors.New("invalid snapshot id")

// ErrPathEscapesRoot is returned when a manifest's relative path would resolve
// outside the project root (or, for backup files, outside the snapshot dir)
// once joined and cleaned — e.g. via a crafted "../../etc/passwd" relPath.
var ErrPathEscapesRoot = errors.New("snapshot manifest path escapes its root")

// validateSnapshotID rejects anything that isn't a bare basename, so it can't
// be used to traverse out of the snapshot store's base directory.
func validateSnapshotID(id string) error {
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSnapshotID, id)
	}
	return nil
}

// safeJoin joins rel onto root and verifies the cleaned result is still
// contained within root, rejecting any ".."-escape (including via an
// absolute rel path, which filepath.Join would otherwise let through
// unchanged on some inputs).
func safeJoin(root, rel string) (string, error) {
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, rel)
	relBack, err := filepath.Rel(cleanRoot, joined)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapesRoot, rel)
	}
	return joined, nil
}

type FileState struct {
	RelPath      string      `json:"relPath"`
	Existed      bool        `json:"existed"`
	FileMode     os.FileMode `json:"fileMode"`
	BackupRelLoc string      `json:"backupRelLoc,omitempty"` // Location relative to snapshot dir
}

type Snapshot struct {
	ID          string               `json:"id"`
	SessionID   string               `json:"sessionId,omitempty"` // the session whose turn this is; "" for snapshots made before it was recorded
	Description string               `json:"description"`
	CreatedAt   time.Time            `json:"createdAt"`
	Files       map[string]FileState `json:"files"` // RelPath -> FileState

	// NextBackupIndex names the next backup file added to this snapshot
	// (file_<NextBackupIndex>.bak). A per-call loop index isn't enough once a
	// snapshot can be reopened and added to across multiple CapturePreMutation
	// calls in the same turn — this persists across those calls so backup
	// filenames never collide.
	NextBackupIndex int `json:"nextBackupIndex,omitempty"`
}

// SnapshotStore captures and restores file state around tool mutations,
// grouped **per turn**: every file a turn touches (via one or more
// CapturePreMutation calls sharing the same turnID) lands in the same
// Snapshot, so one restore_snapshot call can revert an entire turn's changes
// — see SPEC.md §6's "restores modified files to their exact pre-turn state
// and removes any files created during that turn."
type SnapshotStore interface {
	// CapturePreMutation records each absFilePaths' current on-disk state
	// (or its absence) into turnID's snapshot, creating that snapshot on the
	// first call for a given turnID and reusing/extending it on later calls
	// for the same still-open turn. A file already recorded earlier in the
	// same turn is left untouched — its *pre-turn* state must win, not an
	// intermediate one — and doesn't count toward the returned captured
	// bool, which reports whether anything new was actually captured this
	// call (a caller can use it to decide whether a "snapshot created/
	// updated" notification is warranted).
	CapturePreMutation(sessionID, turnID, description string, projectRoot string, absFilePaths ...string) (snap *Snapshot, captured bool, err error)
	// CloseTurn stops tracking turnID as "still open" for grouping purposes.
	// Purely an in-memory bookkeeping eviction (no on-disk effect) — call it
	// once a turn has finished, cancelled or not, so the tracking map
	// doesn't grow for the life of the process.
	CloseTurn(turnID string)
	// RestoreSnapshot returns every file captured by id or any later
	// snapshot of id's session to its state right before id's turn, first
	// capturing their current state as a new snapshot so the restore can
	// itself be undone. It refuses when a file it would revert was also
	// changed by a later snapshot of another session.
	RestoreSnapshot(id string, projectRoot string) error
	// ListSnapshots returns every snapshot, oldest first.
	ListSnapshots() ([]*Snapshot, error)
	GetSnapshot(id string) (*Snapshot, error)
	// ReadBackup returns relPath's content as id captured it; existed is
	// false when the file didn't exist before that turn.
	ReadBackup(id, relPath string) (content []byte, existed bool, err error)
}

type DiskSnapshotStore struct {
	mu      sync.RWMutex
	baseDir string

	// openByTurn maps a still-in-progress turnID to the snapshot ID
	// CapturePreMutation calls for that turn should keep extending, rather
	// than each call creating its own independent snapshot.
	openByTurn map[string]string
}

func NewDiskSnapshotStore(dataDir string) (*DiskSnapshotStore, error) {
	baseDir := filepath.Join(dataDir, "snapshots")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("creating snapshot dir: %w", err)
	}
	return &DiskSnapshotStore{
		baseDir:    baseDir,
		openByTurn: make(map[string]string),
	}, nil
}

func (s *DiskSnapshotStore) CapturePreMutation(sessionID, turnID, description string, projectRoot string, absFilePaths ...string) (*Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureLocked(sessionID, turnID, description, projectRoot, absFilePaths...)
}

func (s *DiskSnapshotStore) captureLocked(sessionID, turnID, description string, projectRoot string, absFilePaths ...string) (*Snapshot, bool, error) {
	snap, snapDir, isNewSnapshot, err := s.openOrCreateTurnSnapshot(sessionID, turnID, description)
	if err != nil {
		return nil, false, err
	}
	if !isNewSnapshot && description != "" && !strings.Contains(snap.Description, description) {
		if snap.Description == "" {
			snap.Description = description
		} else {
			snap.Description += "; " + description
		}
	}

	dataDir := filepath.Join(snapDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, false, err
	}

	captured := false
	for _, absPath := range absFilePaths {
		relPath, err := filepath.Rel(projectRoot, absPath)
		if err != nil {
			return nil, false, fmt.Errorf("calculating relative path: %w", err)
		}

		if _, alreadyTracked := snap.Files[relPath]; alreadyTracked {
			// This turn already recorded this file's pre-turn state —
			// leave it alone so a second mutation in the same turn doesn't
			// overwrite it with an already-mutated intermediate state.
			continue
		}

		// Through an os.Root (normalizer.InRoot): a directory swapped for a
		// symlink since the tool validated absPath can't pull an outside
		// file into the backup, which a restore would then write back in.
		// Opened O_NONBLOCK and checked on the open file: a FIFO planted at
		// the path would otherwise block here, with s.mu held, until a
		// writer shows up. Copied rather than read whole, so a large file
		// isn't held in memory.
		backupName := fmt.Sprintf("file_%d.bak", snap.NextBackupIndex)
		backupPath := filepath.Join(dataDir, backupName)
		var info os.FileInfo
		err = normalizer.InRoot(projectRoot, absPath, func(r *os.Root, rel string) error {
			src, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			defer src.Close()
			if info, err = src.Stat(); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", rel)
			}
			dst, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
			if err != nil {
				return fmt.Errorf("saving backup copy: %w", err)
			}
			if _, err := io.Copy(dst, src); err != nil {
				dst.Close()
				return fmt.Errorf("saving backup copy: %w", err)
			}
			return dst.Close()
		})
		if os.IsNotExist(err) {
			// File does not exist yet; rollback means removing it
			snap.Files[relPath] = FileState{
				RelPath: relPath,
				Existed: false,
			}
			captured = true
			continue
		} else if err != nil {
			return nil, false, fmt.Errorf("capturing %s: %w", absPath, err)
		}
		snap.NextBackupIndex++

		snap.Files[relPath] = FileState{
			RelPath:      relPath,
			Existed:      true,
			FileMode:     info.Mode(),
			BackupRelLoc: filepath.Join("data", backupName),
		}
		captured = true
	}

	if err := s.writeManifest(snapDir, snap); err != nil {
		return nil, false, err
	}

	return snap, captured, nil
}

// openOrCreateTurnSnapshot returns the snapshot currently open for turnID —
// creating a fresh one (and registering it in openByTurn) if this is the
// first call for that turn — along with whether it was just created. Caller
// must hold s.mu.
func (s *DiskSnapshotStore) openOrCreateTurnSnapshot(sessionID, turnID, description string) (*Snapshot, string, bool, error) {
	if existingID, ok := s.openByTurn[turnID]; ok {
		snapDir := filepath.Join(s.baseDir, existingID)
		data, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
		if err == nil {
			var snap Snapshot
			if err := json.Unmarshal(data, &snap); err == nil {
				return &snap, snapDir, false, nil
			}
		}
		// The manifest this turn was tracking is missing or corrupt (e.g.
		// deleted out from under us) — fall through and start a fresh one
		// rather than failing the mutation outright.
	}

	snapID := fmt.Sprintf("snap_%d", time.Now().UnixNano())
	snapDir := filepath.Join(s.baseDir, snapID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, "", false, err
	}

	snap := &Snapshot{
		ID:          snapID,
		SessionID:   sessionID,
		Description: description,
		CreatedAt:   time.Now().UTC(),
		Files:       make(map[string]FileState),
	}
	s.openByTurn[turnID] = snapID

	return snap, snapDir, true, nil
}

func (s *DiskSnapshotStore) writeManifest(snapDir string, snap *Snapshot) error {
	manifestBytes, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return err
	}
	return nil
}

// CloseTurn evicts turnID from the open-turn tracking map — see the
// SnapshotStore interface doc. No-op if turnID was never open (already
// closed, or it never captured anything).
func (s *DiskSnapshotStore) CloseTurn(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.openByTurn, turnID)
}

func (s *DiskSnapshotStore) RestoreSnapshot(id string, projectRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	target, err := s.readManifest(id)
	if err != nil {
		return err
	}
	all, err := s.listLocked()
	if err != nil {
		return err
	}
	// Newest first, down to target: each file ends at the copy taken by the
	// oldest snapshot of target's session (at or after target) that touched
	// it, which is its state right before target's turn. Another session's
	// snapshots are not reverted, so a file one of them changed later can't
	// be restored without undoing that work.
	var apply []*Snapshot
	affected := map[string]bool{}
	laterOther := map[string]string{} // file -> another session's later snapshot that touched it
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].CreatedAt.Before(target.CreatedAt) {
			break
		}
		if all[i].SessionID != target.SessionID {
			for rel := range all[i].Files {
				laterOther[rel] = all[i].ID
			}
			continue
		}
		apply = append(apply, all[i])
		for rel := range all[i].Files {
			affected[rel] = true
		}
	}
	for rel := range affected {
		if other, ok := laterOther[rel]; ok {
			return fmt.Errorf("cannot restore %s: another session changed it after this turn (snapshot %s); restore that snapshot first", filepath.ToSlash(rel), other)
		}
	}

	norm, err := normalizer.New(projectRoot)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(affected))
	for rel := range affected {
		p, err := restoreTarget(norm, rel)
		if err != nil {
			return err
		}
		paths = append(paths, p)
	}
	undoTurn := fmt.Sprintf("restore:%s:%d", id, time.Now().UnixNano())
	_, _, err = s.captureLocked(target.SessionID, undoTurn, "Before restoring to "+target.CreatedAt.Local().Format("Jan 2 15:04:05"), norm.ProjectRoot, paths...)
	delete(s.openByTurn, undoTurn)
	if err != nil {
		return fmt.Errorf("saving an undo snapshot: %w", err)
	}

	for _, snap := range apply {
		if err := s.applyLocked(snap, norm); err != nil {
			return err
		}
	}
	return nil
}

// restoreTarget resolves rel's parent through symlinks and checks it's still
// inside the project, so a link planted after capture can't redirect a
// restore elsewhere. The last component isn't followed: a restore replaces
// whatever sits there.
func restoreTarget(norm *normalizer.Normalizer, rel string) (string, error) {
	if _, err := safeJoin(norm.ProjectRoot, rel); err != nil {
		return "", err
	}
	dir, err := norm.ResolveAndValidatePath(filepath.Dir(rel))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(rel)), nil
}

func (s *DiskSnapshotStore) applyLocked(snap *Snapshot, norm *normalizer.Normalizer) error {
	snapDir := filepath.Join(s.baseDir, snap.ID)
	for relPath, state := range snap.Files {
		targetPath, err := restoreTarget(norm, relPath)
		if err != nil {
			return err
		}
		var originalData []byte
		if state.Existed {
			backupFile, err := safeJoin(snapDir, state.BackupRelLoc)
			if err != nil {
				return err
			}
			if originalData, err = os.ReadFile(backupFile); err != nil {
				return fmt.Errorf("failed to read backup file %s: %w", backupFile, err)
			}
		}
		// Through an os.Root, so a directory swapped for a symlink since
		// restoreTarget checked it can't send the restore out of the project.
		err = normalizer.InRoot(norm.ProjectRoot, targetPath, func(r *os.Root, rel string) error {
			// Replace a symlink rather than write through it.
			if fi, err := r.Lstat(rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if err := r.Remove(rel); err != nil {
					return err
				}
			}
			if !state.Existed {
				if err := r.Remove(rel); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("failed to remove newly created file %s: %w", relPath, err)
				}
				return nil
			}
			if err := r.MkdirAll(filepath.Dir(rel), 0755); err != nil {
				return err
			}
			if err := r.WriteFile(rel, originalData, state.FileMode); err != nil {
				return fmt.Errorf("restoring file %s: %w", targetPath, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *DiskSnapshotStore) ListSnapshots() ([]*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked()
}

func (s *DiskSnapshotStore) listLocked() ([]*Snapshot, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Snapshot{}, nil
		}
		return nil, err
	}

	var snaps []*Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifest := filepath.Join(s.baseDir, e.Name(), "manifest.json")
		data, err := os.ReadFile(manifest)
		if err != nil {
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal(data, &snap); err == nil {
			snaps = append(snaps, &snap)
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.Before(snaps[j].CreatedAt) })
	return snaps, nil
}

func (s *DiskSnapshotStore) readManifest(id string) (*Snapshot, error) {
	if err := validateSnapshotID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.baseDir, id, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot %s not found", id)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("corrupted snapshot manifest: %w", err)
	}
	return &snap, nil
}

func (s *DiskSnapshotStore) ReadBackup(id, relPath string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, err := s.readManifest(id)
	if err != nil {
		return nil, false, err
	}
	state, ok := snap.Files[filepath.FromSlash(relPath)]
	if !ok {
		return nil, false, fmt.Errorf("snapshot %s has no copy of %s", id, relPath)
	}
	if !state.Existed {
		return nil, false, nil
	}
	backup, err := safeJoin(filepath.Join(s.baseDir, id), state.BackupRelLoc)
	if err != nil {
		return nil, false, err
	}
	content, err := os.ReadFile(backup)
	return content, true, err
}

func (s *DiskSnapshotStore) GetSnapshot(id string) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readManifest(id)
}
