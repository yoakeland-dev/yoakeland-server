package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SessionState struct {
	SessionID  string     `json:"sessionId"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	History    []Message  `json:"history"`
	Title      string     `json:"title,omitempty"`
	Archived   bool       `json:"archived,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	// LinkedSessionID pairs this session with another one for a split/sidebar
	// layout (e.g. "session-main" paired with "session-scratch"). Purely a
	// client layout hint — the backend never reads or acts on it.
	LinkedSessionID *string `json:"linkedSessionId,omitempty"`

	// Mode mirrors the live Agent's current Mode (empty means the default,
	// ModeReadWrite). Persisted — unlike the in-memory-only Agent.mode this
	// backs — specifically so a peer connecting or reconnecting after
	// another peer already changed it (get_session_history, §3.1) reads the
	// true current value from disk instead of a stale client-side guess.
	// Kept in sync with the live Agent by SetMode itself (agent.go).
	Mode string `json:"mode,omitempty"`

	// Branches holds every suffix that used to be active in History but got
	// swapped out by an edit_message/retry_message, keyed by the ID of the
	// message immediately before the branch point ("" for the start of the
	// session). Never pruned — an edit or retry only ever stashes here, it
	// never deletes, so a branch that already cost real money to generate is
	// always recoverable via switch_branch. See agent.go's Branches doc.
	Branches map[string][][]Message `json:"branches,omitempty"`

	// NextMsgSeq is the counter behind every Message.ID minted for this
	// session ("m1", "m2", ...). Persisted explicitly rather than derived
	// from len(History) because Branches also consumes IDs that may no
	// longer appear in History at all.
	NextMsgSeq int `json:"nextMsgSeq,omitempty"`
}

type SessionStore interface {
	Save(session *SessionState) error
	Load(sessionID string) (*SessionState, error)
	List() ([]string, error)
	Delete(sessionID string) error
	// Update applies mutate to a session's state and saves the result as one
	// atomic read-modify-write, creating the file if it doesn't exist yet.
	Update(sessionID string, mutate func(*SessionState) error) (*SessionState, error)
	// UpdateExisting is Update for callers that require a session to already
	// have persisted state, returning ErrSessionNotFound rather than creating
	// one.
	UpdateExisting(sessionID string, mutate func(*SessionState) error) (*SessionState, error)
	// Ensure creates an empty session file if there isn't one, and does nothing
	// if there is.
	Ensure(sessionID string) error
}

// ErrSessionNotFound distinguishes "this id has no persisted file yet" from a
// real read failure. The distinction matters because a session that hasn't
// been saved yet is an ordinary state — a brand-new chat, or any session
// whose first turn hasn't reached the model — not an error to report.
var ErrSessionNotFound = errors.New("session not found")

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
	return s.saveLocked(session)
}

// saveLocked is Save's body without the lock, so Update can hold one lock
// across its whole read-modify-write. Writing to a .tmp file and renaming over
// the target means a reader never observes a half-written session; the
// directory is fsynced afterward so the rename itself survives a crash.
func (s *DiskSessionStore) saveLocked(session *SessionState) error {
	session.UpdatedAt = time.Now().UTC()
	bytes, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	target := filepath.Join(s.baseDir, session.SessionID+".json")
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(bytes); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	if dir, err := os.Open(s.baseDir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *DiskSessionStore) Load(sessionID string) (*SessionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadLocked(sessionID)
}

func (s *DiskSessionStore) loadLocked(sessionID string) (*SessionState, error) {
	target := filepath.Join(s.baseDir, sessionID+".json")
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %s: %w", sessionID, ErrSessionNotFound)
		}
		return nil, err
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("corrupted session data: %w", err)
	}
	return &state, nil
}

// Update reads a session, applies mutate, and writes the result back with one
// lock held for the whole sequence. Every writer of a session file does a
// read-modify-write (a finishing turn replacing History, a set_session_*
// handler changing Title/Archived/LinkedSessionID), and the pairs are not
// individually atomic: two of them interleaving would let the later write
// resurrect the earlier reader's stale copy of the fields it didn't mean to
// touch — a turn's worth of messages, or a title, silently reverting.
//
// A session with no file yet is passed a fresh state (SessionID/CreatedAt
// filled in, empty History) rather than failing, since a new chat's first turn
// is the first thing that ever writes one.
func (s *DiskSessionStore) Update(sessionID string, mutate func(*SessionState) error) (*SessionState, error) {
	return s.update(sessionID, mutate, true)
}

// UpdateExisting is Update for a caller that requires the session to already
// have persisted state — set_session_title and friends, which SPEC.md §3.1
// defines as an error on a session with no history (and which the turn's own
// write-ahead has normally already created by then). Same atomicity; the only
// difference is that a missing file is ErrSessionNotFound rather than a new
// session.
func (s *DiskSessionStore) UpdateExisting(sessionID string, mutate func(*SessionState) error) (*SessionState, error) {
	return s.update(sessionID, mutate, false)
}

func (s *DiskSessionStore) update(sessionID string, mutate func(*SessionState) error, create bool) (*SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.loadLocked(sessionID)
	if err != nil {
		if !errors.Is(err, ErrSessionNotFound) || !create {
			return nil, err
		}
		state = &SessionState{
			SessionID: sessionID,
			CreatedAt: time.Now().UTC(),
			History:   make([]Message, 0),
		}
	}

	if err := mutate(state); err != nil {
		return nil, err
	}
	if err := s.saveLocked(state); err != nil {
		return nil, err
	}
	return state, nil
}

// Ensure creates an empty session file for sessionID if there isn't one, and
// leaves an existing file untouched (no rewrite, no UpdatedAt bump). A prompt
// calls this before spawning its turn so that the very next envelope on the
// same connection — the set_session_title a client pushes alongside a first
// message — finds a session that exists instead of racing the turn's own first
// write and being rejected as "not found" (SPEC.md §3.1).
func (s *DiskSessionStore) Ensure(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadLocked(sessionID); err == nil {
		return nil
	} else if !errors.Is(err, ErrSessionNotFound) {
		return err
	}
	return s.saveLocked(&SessionState{
		SessionID: sessionID,
		CreatedAt: time.Now().UTC(),
		History:   make([]Message, 0),
	})
}

func (s *DiskSessionStore) Delete(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	target := filepath.Join(s.baseDir, sessionID+".json")
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session %s not found", sessionID)
		}
		return err
	}
	return nil
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
