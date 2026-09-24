package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type PersistedRule struct {
	Allowed   bool      `json:"allowed"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type FilePermissionsModel struct {
	Version int                      `json:"version"`
	Rules   map[string]PersistedRule `json:"rules"` // cacheKey -> rule
}

type PersistentStore struct {
	mu       sync.RWMutex
	filePath string
}

func NewPersistentStore(projectRoot string) *PersistentStore {
	dir := filepath.Join(projectRoot, ".yoake")
	_ = os.MkdirAll(dir, 0755)
	return &PersistentStore{
		filePath: filepath.Join(dir, "permissions.json"),
	}
}

func (s *PersistentStore) Load() (map[string]bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]bool), nil
		}
		return nil, err
	}

	var model FilePermissionsModel
	if err := json.Unmarshal(data, &model); err != nil {
		return nil, fmt.Errorf("malformed permissions.json: %w", err)
	}

	result := make(map[string]bool)
	for k, v := range model.Rules {
		result[k] = v.Allowed
	}
	return result, nil
}

func (s *PersistentStore) Save(rules map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	model := FilePermissionsModel{
		Version: 1,
		Rules:   make(map[string]PersistedRule),
	}

	for k, v := range rules {
		model.Rules[k] = PersistedRule{
			Allowed:   v,
			UpdatedAt: time.Now().UTC(),
		}
	}

	bytes, err := json.MarshalIndent(model, "", "  ")
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
