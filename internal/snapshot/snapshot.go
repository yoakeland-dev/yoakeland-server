package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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
}

type SnapshotStore interface {
	CapturePreMutation(description string, projectRoot string, absFilePaths ...string) (*Snapshot, error)
	RestoreSnapshot(id string, projectRoot string) error
	ListSnapshots() ([]*Snapshot, error)
	GetSnapshot(id string) (*Snapshot, error)
}

type DiskSnapshotStore struct {
	mu          sync.RWMutex
	baseDir     string
	manifestDir string
}

func NewDiskSnapshotStore(projectRoot string) (*DiskSnapshotStore, error) {
	baseDir := filepath.Join(projectRoot, ".yoake", "snapshots")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("creating snapshot dir: %w", err)
	}
	return &DiskSnapshotStore{
		baseDir:     baseDir,
		manifestDir: baseDir,
	}, nil
}

func (s *DiskSnapshotStore) CapturePreMutation(description string, projectRoot string, absFilePaths ...string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapID := fmt.Sprintf("snap_%d", time.Now().UnixNano())
	snapDir := filepath.Join(s.baseDir, snapID)
	dataDir := filepath.Join(snapDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	snap := &Snapshot{
		ID:          snapID,
		Description: description,
		CreatedAt:   time.Now().UTC(),
		Files:       make(map[string]FileState),
	}

	for i, absPath := range absFilePaths {
		relPath, err := filepath.Rel(projectRoot, absPath)
		if err != nil {
			return nil, fmt.Errorf("calculating relative path: %w", err)
		}

		info, err := os.Stat(absPath)
		if os.IsNotExist(err) {
			// File does not exist yet; rollback means removing it
			snap.Files[relPath] = FileState{
				RelPath: relPath,
				Existed: false,
			}
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat failed on %s: %w", absPath, err)
		}

		content, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("reading file for snapshot: %w", err)
		}

		backupName := fmt.Sprintf("file_%d.bak", i)
		backupPath := filepath.Join(dataDir, backupName)
		if err := os.WriteFile(backupPath, content, info.Mode()); err != nil {
			return nil, fmt.Errorf("saving backup copy: %w", err)
		}

		snap.Files[relPath] = FileState{
			RelPath:      relPath,
			Existed:      true,
			FileMode:     info.Mode(),
			BackupRelLoc: filepath.Join("data", backupName),
		}
	}

	manifestBytes, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return nil, err
	}

	return snap, nil
}

func (s *DiskSnapshotStore) RestoreSnapshot(id string, projectRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
		targetPath := filepath.Join(projectRoot, relPath)

		if !state.Existed {
			// File was newly created after snapshot; remove it
			if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove newly created file %s: %w", relPath, err)
			}
			continue
		}

		backupFile := filepath.Join(snapDir, state.BackupRelLoc)
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
