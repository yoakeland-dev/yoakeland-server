package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	CapturePreMutation(turnID, description string, projectRoot string, absFilePaths ...string) (snap *Snapshot, captured bool, err error)
	// CloseTurn stops tracking turnID as "still open" for grouping purposes.
	// Purely an in-memory bookkeeping eviction (no on-disk effect) — call it
	// once a turn has finished, cancelled or not, so the tracking map
	// doesn't grow for the life of the process.
	CloseTurn(turnID string)
	RestoreSnapshot(id string, projectRoot string) error
	ListSnapshots() ([]*Snapshot, error)
	GetSnapshot(id string) (*Snapshot, error)
}

type DiskSnapshotStore struct {
	mu          sync.RWMutex
	baseDir     string
	manifestDir string

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
		baseDir:     baseDir,
		manifestDir: baseDir,
		openByTurn:  make(map[string]string),
	}, nil
}

func (s *DiskSnapshotStore) CapturePreMutation(turnID, description string, projectRoot string, absFilePaths ...string) (*Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, snapDir, isNewSnapshot, err := s.openOrCreateTurnSnapshot(turnID, description)
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

		info, err := os.Stat(absPath)
		if os.IsNotExist(err) {
			// File does not exist yet; rollback means removing it
			snap.Files[relPath] = FileState{
				RelPath: relPath,
				Existed: false,
			}
			captured = true
			continue
		} else if err != nil {
			return nil, false, fmt.Errorf("stat failed on %s: %w", absPath, err)
		}

		content, err := os.ReadFile(absPath)
		if err != nil {
			return nil, false, fmt.Errorf("reading file for snapshot: %w", err)
		}

		backupName := fmt.Sprintf("file_%d.bak", snap.NextBackupIndex)
		snap.NextBackupIndex++
		backupPath := filepath.Join(dataDir, backupName)
		if err := os.WriteFile(backupPath, content, info.Mode()); err != nil {
			return nil, false, fmt.Errorf("saving backup copy: %w", err)
		}

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
func (s *DiskSnapshotStore) openOrCreateTurnSnapshot(turnID, description string) (*Snapshot, string, bool, error) {
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

	if err := validateSnapshotID(id); err != nil {
		return err
	}
	snapDir := filepath.Join(s.baseDir, id)
	manifestPath := filepath.Join(snapDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("snapshot %s not found: %w", id, err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("corrupted snapshot manifest: %w", err)
	}

	for relPath, state := range snap.Files {
		targetPath, err := safeJoin(projectRoot, relPath)
		if err != nil {
			return err
		}

		if !state.Existed {
			// File was newly created after snapshot; remove it
			if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove newly created file %s: %w", relPath, err)
			}
			continue
		}

		backupFile, err := safeJoin(snapDir, state.BackupRelLoc)
		if err != nil {
			return err
		}
		originalData, err := os.ReadFile(backupFile)
		if err != nil {
			return fmt.Errorf("failed to read backup file %s: %w", backupFile, err)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(targetPath, originalData, state.FileMode); err != nil {
			return fmt.Errorf("restoring file %s: %w", targetPath, err)
		}
	}

	return nil
}

func (s *DiskSnapshotStore) ListSnapshots() ([]*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
	return snaps, nil
}

func (s *DiskSnapshotStore) GetSnapshot(id string) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := validateSnapshotID(id); err != nil {
		return nil, err
	}
	manifest := filepath.Join(s.baseDir, id, "manifest.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s not found", id)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
