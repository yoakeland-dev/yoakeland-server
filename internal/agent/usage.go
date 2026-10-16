package agent

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"yoakeland/internal/fsutil"
)

// UsageTotals is a running tally of token spend, either for one session or
// for the whole project.
type UsageTotals struct {
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	// CostUnknown: some usage in the tally had no known price, so CostUSD
	// is what could be priced, not everything spent.
	CostUnknown bool `json:"costUnknown,omitempty"`
}

type usageModel struct {
	Sessions map[string]UsageTotals `json:"sessions"`
	Project  UsageTotals            `json:"project"`
}

// UsageStore persists running token/cost totals in usage.json,
// following the same pattern as DiskSessionStore and PersistentStore.
type UsageStore struct {
	mu       sync.Mutex
	filePath string
	model    usageModel
}

func NewUsageStore(dataDir string) (*UsageStore, error) {
	dir := dataDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &UsageStore{
		filePath: filepath.Join(dir, "usage.json"),
		model:    usageModel{Sessions: make(map[string]UsageTotals)},
	}

	if data, err := os.ReadFile(s.filePath); err == nil {
		if json.Unmarshal(data, &s.model) != nil {
			// Keep the unreadable file rather than let the next save destroy it.
			if err := os.Rename(s.filePath, s.filePath+".corrupt"); err != nil {
				log.Printf("usage: corrupt %s, could not set it aside: %v", s.filePath, err)
			} else {
				log.Printf("usage: corrupt %s moved to %s.corrupt; starting empty", s.filePath, s.filePath)
			}
			s.model = usageModel{Sessions: make(map[string]UsageTotals)}
		}
	}
	if s.model.Sessions == nil {
		s.model.Sessions = make(map[string]UsageTotals)
	}
	return s, nil
}

// SessionTotal returns a session's running total without recording anything
// new, so a caller can check it against a spend cap before starting a turn.
func (s *UsageStore) SessionTotal(sessionID string) UsageTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model.Sessions[sessionID]
}

// ProjectTotal returns the project-wide running total.
func (s *UsageStore) ProjectTotal() UsageTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model.Project
}

// MarkCostUnknown notes that usage with no known price was (or is about to
// be) recorded for sessionID, so its totals read as a lower bound.
func (s *UsageStore) MarkCostUnknown(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.model.Sessions[sessionID]
	sess.CostUnknown = true
	s.model.Sessions[sessionID] = sess
	s.model.Project.CostUnknown = true
}

// Record adds a turn's token usage and cost to both the session's running
// total and the project-wide total, persists them, and returns both updated
// totals.
func (s *UsageStore) Record(sessionID string, inputTokens, outputTokens int, costUSD float64) (session, project UsageTotals) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.model.Sessions[sessionID]
	sess.InputTokens += inputTokens
	sess.OutputTokens += outputTokens
	sess.CostUSD += costUSD
	s.model.Sessions[sessionID] = sess

	s.model.Project.InputTokens += inputTokens
	s.model.Project.OutputTokens += outputTokens
	s.model.Project.CostUSD += costUSD

	_ = s.save()
	return sess, s.model.Project
}

func (s *UsageStore) save() error {
	data, err := json.MarshalIndent(s.model, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.filePath, data, 0644)
}
