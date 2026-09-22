// Package settings persists small per-project toggles under .yoake/ — today
// just whether the web_search/web_fetch tools are exposed to the model
// (see SPEC.md §4c's mode discussion for why this is a separate, orthogonal
// setting rather than folded into mode itself). Load-whole-file/
// save-via-temp-then-rename, the same shape permission.PersistentStore uses.
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

// NewStore returns a settings store rooted at <projectRoot>/.yoake/settings.json.
func NewStore(projectRoot string) *Store {
	dir := filepath.Join(projectRoot, ".yoake")
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
	if err := os.WriteFile(tmpFile, bytes, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}
