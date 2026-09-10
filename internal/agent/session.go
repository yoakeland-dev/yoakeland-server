package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SessionState struct {
	SessionID string    `json:"sessionId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	History   []Message `json:"history"`
}

type SessionStore interface {
	Save(session *SessionState) error
	Load(sessionID string) (*SessionState, error)
	List() ([]string, error)
}

type DiskSessionStore struct {
	mu      sync.RWMutex
	baseDir string
}

func NewDiskSessionStore(projectRoot string) (*DiskSessionStore, error) {
	dir := filepath.Join(projectRoot, ".yoake", "sessions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	return &DiskSessionStore{baseDir: dir}, nil
}

func (s *DiskSessionStore) Save(session *SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session.UpdatedAt = time.Now().UTC()
	bytes, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	target := filepath.Join(s.baseDir, session.SessionID+".json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, bytes, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func (s *DiskSessionStore) Load(sessionID string) (*SessionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	target := filepath.Join(s.baseDir, sessionID+".json")
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %s not found", sessionID)
		}
		return nil, err
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("corrupted session data: %w", err)
	}
	return &state, nil
}

func (s *DiskSessionStore) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			ids = append(ids, e.Name()[:len(e.Name())-5])
		}
	}
	return ids, nil
}
