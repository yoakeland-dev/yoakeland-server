// Package settings persists per-project toggles: currently whether
// web_search/web_fetch are exposed (SPEC.md §4c says why that isn't a mode).
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type ProjectSettings struct {
	Version         int  `json:"version"`
	WebToolsEnabled bool `json:"webToolsEnabled"`
}

type Store struct {
	mu       sync.RWMutex
	filePath string
}

func NewStore(dataDir string) *Store {
	dir := dataDir
	_ = os.MkdirAll(dir, 0755)
	return &Store{
		filePath: filepath.Join(dir, "settings.json"),
	}
}

// Load returns the persisted settings, or the default (web tools enabled)
// if the file doesn't exist yet.
func (s *Store) Load() (ProjectSettings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return ProjectSettings{Version: 1, WebToolsEnabled: true}, nil
		}
		return ProjectSettings{}, err
	}

	var model ProjectSettings
	if err := json.Unmarshal(data, &model); err != nil {
		return ProjectSettings{}, fmt.Errorf("malformed settings.json: %w", err)
	}
	return model, nil
}

func (s *Store) Save(settings ProjectSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bytes, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmpFile), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(tmpFile, bytes, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}
