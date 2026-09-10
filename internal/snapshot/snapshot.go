package snapshot

import (
	"fmt"
	"sync"
	"time"
)

type Snapshot struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

type SnapshotStore interface {
	CreateSnapshot(description string) (*Snapshot, error)
	RestoreSnapshot(id string) error
	ListSnapshots() ([]*Snapshot, error)
}

// MemorySnapshotStore is an in-memory mock/stub implementation.
type MemorySnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]*Snapshot
	history   []*Snapshot
}

func NewMemorySnapshotStore() *MemorySnapshotStore {
	return &MemorySnapshotStore{
		snapshots: make(map[string]*Snapshot),
		history:   make([]*Snapshot, 0),
	}
}

func (s *MemorySnapshotStore) CreateSnapshot(desc string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := &Snapshot{
		ID:          fmt.Sprintf("snap-%d", time.Now().UnixNano()),
		Description: desc,
		CreatedAt:   time.Now(),
	}
	s.snapshots[snap.ID] = snap
	s.history = append(s.history, snap)
	return snap, nil
}

func (s *MemorySnapshotStore) RestoreSnapshot(id string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, exists := s.snapshots[id]; !exists {
		return fmt.Errorf("snapshot %s not found", id)
	}
	// Restoration mechanics will interact with file write rollback in subsequent phases
	return nil
}

func (s *MemorySnapshotStore) ListSnapshots() ([]*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Snapshot, len(s.history))
	copy(result, s.history)
	return result, nil
}
