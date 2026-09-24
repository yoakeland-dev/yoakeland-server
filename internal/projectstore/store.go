// Package projectstore persists the server's list of known project paths —
// the one piece of "which projects exist" that used to live only in a
// client's own storage. It says nothing about a project's live state
// (sandbox, sessions, permissions); that's ProjectRegistry's in-memory-only
// concern in cmd/yoakeland, unchanged. This is deliberately narrow: a project
// path, when it was first opened, and when it was last opened — nothing
// else.
package projectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DataDir holds a project's state, outside the project. The id hashes the
// canonical root, so re-adding a removed project finds its history; after a
// move, rename the folder to the new id.
func DataDir(stateDir, root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(stateDir, "projects", hex.EncodeToString(sum[:8]))
}

// KnownProject is one entry in the persisted list.
type KnownProject struct {
	Path         string    `json:"path"`
	AddedAt      time.Time `json:"addedAt"`
	LastOpenedAt time.Time `json:"lastOpenedAt"`
}

type fileModel struct {
	Version  int            `json:"version"`
	Projects []KnownProject `json:"projects"`
}

// Store is a small JSON file under the server's -state-dir, alongside
// providers.json — server-level state that belongs to whoever runs the
// server, not to any one project. Same load-whole-file/save-whole-file
// pattern as permission.PersistentStore: not built for high write volume,
// and doesn't need to be (a project is added/opened/removed, not written
// continuously).
type Store struct {
	mu       sync.RWMutex
	filePath string
}

// New opens (or creates) the project-list store in dir (the server's
// -state-dir).
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating project store dir: %w", err)
	}
	return &Store{filePath: filepath.Join(dir, "projects.json")}, nil
}

func (s *Store) load() (fileModel, error) {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fileModel{Version: 1}, nil
		}
		return fileModel{}, err
	}
	var model fileModel
	if err := json.Unmarshal(data, &model); err != nil {
		return fileModel{}, fmt.Errorf("malformed projects.json: %w", err)
	}
	return model, nil
}

func (s *Store) save(model fileModel) error {
	model.Version = 1
	bytes, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, bytes, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}

// List returns every known project, most-recently-opened first.
func (s *Store) List() ([]KnownProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	model, err := s.load()
	if err != nil {
		return nil, err
	}
	out := append([]KnownProject(nil), model.Projects...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastOpenedAt.After(out[j].LastOpenedAt)
	})
	return out, nil
}

// Upsert records path as known, bumping LastOpenedAt (and setting AddedAt on
// first sight). path must already be canonicalized by the caller — this
// store does no path normalization of its own, the same way permissions.json
// trusts its caller's cacheKey.
func (s *Store) Upsert(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	model, err := s.load()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range model.Projects {
		if model.Projects[i].Path == path {
			model.Projects[i].LastOpenedAt = now
			return s.save(model)
		}
	}
	model.Projects = append(model.Projects, KnownProject{Path: path, AddedAt: now, LastOpenedAt: now})
	return s.save(model)
}

// Remove drops path from the known list. Removing a path that isn't there is
// not an error — same idempotency precedent as remove_provider/remove_token.
func (s *Store) Remove(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	model, err := s.load()
	if err != nil {
		return err
	}
	out := model.Projects[:0]
	for _, p := range model.Projects {
		if p.Path != path {
			out = append(out, p)
		}
	}
	model.Projects = out
	return s.save(model)
}
