// Package sched is the server's schedule of turns it starts itself (SPEC.md §3.1
// schedule_turn): artifact submits, timers, repeats, and turns held back until a rate limit
// resets. It only decides when something is due and keeps the list; starting the turn is the
// caller's Run.
package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/protocol"
)

// Limits that keep an unattended schedule from running away.
const (
	MinRepeat          = time.Minute
	DefaultRepeatRuns  = 10
	MaxPerSession      = 20
	MaxTotal           = 200
	MaxTextBytes       = 20000
	keepFinished       = 7 * 24 * time.Hour
	tickEvery          = 5 * time.Second
	limitRecheckEvery  = 30 * time.Second
	fileName           = "schedules.json"
	restartInterrupted = "interrupted by a server restart"
)

// Scheduler holds the schedule and fires what is due.
type Scheduler struct {
	// Run starts a turn for t and returns without waiting for it; finished is called when that
	// turn ends. An error means no turn was started.
	Run func(t protocol.ScheduledTurn, finished func()) error
	// Blocked reports whether a limit_reset entry is still held back by a rate limit.
	Blocked func(t protocol.ScheduledTurn) bool
	// Changed is called (without the lock held) after the list changed, so it can be pushed.
	Changed func()

	now  func() time.Time
	path string

	mu        sync.Mutex
	items     []protocol.ScheduledTurn
	allPaused bool
	active    map[string]bool
	nextID    int
	lastCheck map[string]time.Time
	hits      map[string]protocol.LimitHit
	bindings  map[string]Binding
}

// Binding is the provider and model a chat last ran on. A chat's provider lives in memory only (its
// client re-sends it on opening the chat), so a turn the server starts itself after a restart needs it
// from here.
type Binding struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

type diskState struct {
	Items     []protocol.ScheduledTurn `json:"items"`
	AllPaused bool                     `json:"allPaused,omitempty"`
	Hits      []protocol.LimitHit      `json:"hits,omitempty"`
	Bindings  map[string]Binding       `json:"bindings,omitempty"`
}

// New loads the schedule kept under dir (none yet is fine).
func New(dir string) *Scheduler {
	s := &Scheduler{now: time.Now, path: filepath.Join(dir, fileName), active: map[string]bool{}, lastCheck: map[string]time.Time{}, hits: map[string]protocol.LimitHit{}, bindings: map[string]Binding{}}
	s.load()
	return s
}

func (s *Scheduler) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d diskState
	if json.Unmarshal(b, &d) != nil {
		return
	}
	s.allPaused = d.AllPaused
	for _, h := range d.Hits {
		s.hits[h.SessionID] = h
	}
	for k, b := range d.Bindings {
		s.bindings[k] = b
	}
	for _, t := range d.Items {
		if t.Status == protocol.ScheduleRunning { // its turn died with the old process
			if t.Trigger.Kind == protocol.TriggerRepeat {
				t.Status = protocol.SchedulePending
			} else {
				t.Status, t.Error = protocol.ScheduleDone, restartInterrupted
			}
		}
		s.items = append(s.items, t)
		if n := idNumber(t.ID); n > s.nextID {
			s.nextID = n
		}
	}
}

func idNumber(id string) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(id, "sch-"), "%d", &n)
	return n
}

// save writes the list; the caller holds the lock. A failed write only loses restart safety.
func (s *Scheduler) save() {
	b, err := json.Marshal(diskState{Items: s.items, AllPaused: s.allPaused, Hits: s.hitsLocked(), Bindings: s.bindings})
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *Scheduler) changed() {
	if s.Changed != nil {
		s.Changed()
	}
}

// Add validates and arms a turn, returning the stored entry.
func (s *Scheduler) Add(d protocol.ScheduleTurnData) (protocol.ScheduledTurn, error) {
	text := strings.TrimSpace(d.Text)
	switch {
	case d.SessionID == "":
		return protocol.ScheduledTurn{}, errors.New("a scheduled turn needs a sessionId")
	case text == "":
		return protocol.ScheduledTurn{}, errors.New("a scheduled turn needs text")
	case len(d.Text) > MaxTextBytes:
		return protocol.ScheduledTurn{}, fmt.Errorf("text is over %d bytes", MaxTextBytes)
	}
	now := s.now()
	t := protocol.ScheduledTurn{
		SessionID: d.SessionID, ProjectPath: d.ProjectPath, Text: d.Text, Trigger: d.Trigger,
		Status: protocol.SchedulePending, CreatedAtMs: now.UnixMilli(),
	}
	t.Origin = protocol.Origin{Kind: protocol.OriginScheduled}
	if d.Origin != nil && (d.Origin.Kind == protocol.OriginArtifact || d.Origin.Kind == protocol.OriginScheduled) {
		t.Origin = *d.Origin
	}
	switch d.Trigger.Kind {
	case protocol.TriggerNow:
		t.NextRunAtMs = now.UnixMilli()
	case protocol.TriggerAt:
		if d.Trigger.AtMs <= 0 {
			return t, errors.New("trigger \"at\" needs atMs")
		}
		t.NextRunAtMs = d.Trigger.AtMs
	case protocol.TriggerLimitReset:
		// Due once atMs (if given) has passed and nothing holds it back; with no atMs the time
		// is unknown until a check finds the limit lifted.
		t.NextRunAtMs = d.Trigger.AtMs
		t.Attempt = d.Attempt
	case protocol.TriggerRepeat:
		if time.Duration(d.Trigger.EveryMs)*time.Millisecond < MinRepeat {
			return t, fmt.Errorf("a repeat must be at least %s apart", MinRepeat)
		}
		if t.Trigger.MaxRuns <= 0 || t.Trigger.MaxRuns > DefaultRepeatRuns {
			t.Trigger.MaxRuns = DefaultRepeatRuns
		}
		t.NextRunAtMs = now.UnixMilli() + d.Trigger.EveryMs
	default:
		return t, fmt.Errorf("unknown trigger %q", d.Trigger.Kind)
	}
	t.Origin.Trigger = t.Trigger.Kind

	s.mu.Lock()
	pending := 0
	for _, o := range s.items {
		if o.SessionID == t.SessionID && live(o.Status) {
			pending++
		}
	}
	if pending >= MaxPerSession {
		s.mu.Unlock()
		return t, fmt.Errorf("this chat already has %d scheduled turns", MaxPerSession)
	}
	if len(s.items) >= MaxTotal {
		s.pruneLocked(now, true)
		if len(s.items) >= MaxTotal {
			s.mu.Unlock()
			return t, errors.New("too many scheduled turns")
		}
	}
	s.nextID++
	t.ID = fmt.Sprintf("sch-%d", s.nextID)
	t.Origin.ScheduleID = t.ID
	s.items = append(s.items, t)
	s.save()
	s.mu.Unlock()
	s.changed()
	return t, nil
}

func live(status string) bool {
	return status == protocol.SchedulePending || status == protocol.SchedulePaused || status == protocol.ScheduleRunning
}

// List returns the schedule, newest last; sessionID filters to one chat when set.
func (s *Scheduler) List(sessionID string) protocol.ScheduleListData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := protocol.ScheduleListData{SessionID: sessionID, Schedules: []protocol.ScheduledTurn{}, AllPaused: s.allPaused, LimitHits: s.hitsLocked()}
	for _, t := range s.items {
		if sessionID == "" || t.SessionID == sessionID {
			out.Schedules = append(out.Schedules, t)
		}
	}
	return out
}

// Cancel stops an entry from running again. A turn already running isn't touched (cancel_turn does that).
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	t := s.findLocked(id)
	if t == nil {
		s.mu.Unlock()
		return fmt.Errorf("no scheduled turn %q", id)
	}
	if !live(t.Status) {
		s.mu.Unlock()
		return nil
	}
	t.Status = protocol.ScheduleCancelled
	s.save()
	s.mu.Unlock()
	s.changed()
	return nil
}

// Pause holds one entry (or, with all, the whole schedule) back, or lets it go again.
func (s *Scheduler) Pause(id string, all, paused bool) error {
	s.mu.Lock()
	if all {
		s.allPaused = paused
	} else {
		t := s.findLocked(id)
		if t == nil {
			s.mu.Unlock()
			return fmt.Errorf("no scheduled turn %q", id)
		}
		switch {
		case paused && t.Status == protocol.SchedulePending:
			t.Status = protocol.SchedulePaused
		case !paused && t.Status == protocol.SchedulePaused:
			t.Status = protocol.SchedulePending
		}
	}
	s.save()
	s.mu.Unlock()
	s.changed()
	return nil
}

// Forget drops everything scheduled for a deleted chat.
func (s *Scheduler) Forget(sessionID string) {
	s.mu.Lock()
	kept := s.items[:0]
	for _, t := range s.items {
		if t.SessionID != sessionID {
			kept = append(kept, t)
		}
	}
	dropped := len(kept) != len(s.items)
	s.items = kept
	if _, ok := s.hits[sessionID]; ok {
		delete(s.hits, sessionID)
		dropped = true
	}
	if _, ok := s.bindings[sessionID]; ok {
		delete(s.bindings, sessionID)
		dropped = true
	}
	if dropped {
		s.save()
	}
	s.mu.Unlock()
	if dropped {
		s.changed()
	}
}

func (s *Scheduler) findLocked(id string) *protocol.ScheduledTurn {
	for i := range s.items {
		if s.items[i].ID == id {
			return &s.items[i]
		}
	}
	return nil
}

func (s *Scheduler) pruneLocked(now time.Time, force bool) {
	kept := s.items[:0]
	for _, t := range s.items {
		old := !live(t.Status) && now.Sub(time.UnixMilli(t.CreatedAtMs)) > keepFinished
		if !live(t.Status) && (old || force) {
			continue
		}
		kept = append(kept, t)
	}
	s.items = kept
}

// Tick starts every entry that is due. Start calls it every few seconds.
func (s *Scheduler) Tick() {
	now := s.now()
	s.mu.Lock()
	if s.allPaused || s.Run == nil {
		s.mu.Unlock()
		return
	}
	var due, check []protocol.ScheduledTurn
	for _, t := range s.items {
		if t.Status != protocol.SchedulePending || s.active[t.ID] {
			continue
		}
		if t.NextRunAtMs > now.UnixMilli() {
			continue
		}
		if t.Trigger.Kind == protocol.TriggerLimitReset && s.Blocked != nil {
			// Asked outside the lock (Blocked looks at other state, the scheduler's own included),
			// and not more than every limitRecheckEvery.
			if last, ok := s.lastCheck[t.ID]; ok && now.Sub(last) < limitRecheckEvery {
				continue
			}
			s.lastCheck[t.ID] = now
			check = append(check, t)
			continue
		}
		due = append(due, t)
	}
	s.mu.Unlock()
	for _, t := range check {
		if !s.Blocked(t) {
			due = append(due, t)
		}
	}
	// Oldest first, so two entries for one chat run in the order they were armed.
	sort.SliceStable(due, func(i, j int) bool { return due[i].CreatedAtMs < due[j].CreatedAtMs })
	changed := false
	for _, t := range due {
		changed = s.fire(t) || changed
	}
	s.mu.Lock()
	before := len(s.items)
	s.pruneLocked(now, false)
	if len(s.items) != before {
		s.save()
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.changed()
	}
}

// fire starts t and records what happened; reports whether the entry changed.
func (s *Scheduler) fire(t protocol.ScheduledTurn) bool {
	s.mu.Lock()
	cur := s.findLocked(t.ID)
	if cur == nil || cur.Status != protocol.SchedulePending {
		s.mu.Unlock()
		return false
	}
	cur.Status, cur.Error = protocol.ScheduleRunning, ""
	s.active[t.ID] = true
	s.mu.Unlock()

	err := s.Run(t, func() { s.finish(t.ID) })

	s.mu.Lock()
	defer s.mu.Unlock()
	cur = s.findLocked(t.ID)
	if cur == nil {
		return true
	}
	if err != nil {
		delete(s.active, t.ID)
		cur.Error = err.Error()
		cur.Runs++
		s.advanceLocked(cur)
		if cur.Status == protocol.ScheduleRunning {
			cur.Status = protocol.ScheduleDone
		}
	}
	s.save()
	return true
}

// finish is the turn's end: a one-off is done, a repeat waits for its next time.
func (s *Scheduler) finish(id string) {
	s.mu.Lock()
	delete(s.active, id)
	t := s.findLocked(id)
	if t != nil && t.Status == protocol.ScheduleRunning {
		t.Runs++
		s.advanceLocked(t)
		s.save()
	}
	s.mu.Unlock()
	s.changed()
}

// advanceLocked moves an entry that just ran to what comes next.
func (s *Scheduler) advanceLocked(t *protocol.ScheduledTurn) {
	if t.Trigger.Kind == protocol.TriggerRepeat && t.Runs < t.Trigger.MaxRuns && t.Error == "" {
		t.Status = protocol.SchedulePending
		t.NextRunAtMs = s.now().UnixMilli() + t.Trigger.EveryMs
		return
	}
	t.Status = protocol.ScheduleDone
	t.NextRunAtMs = 0
}

// Start runs Tick until ctx ends.
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		tk := time.NewTicker(tickEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				s.Tick()
			}
		}
	}()
}

// hitsLocked lists the limit hits oldest first; the caller holds the lock.
func (s *Scheduler) hitsLocked() []protocol.LimitHit {
	out := make([]protocol.LimitHit, 0, len(s.hits))
	for _, h := range s.hits {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

// SetHit records that a chat stopped at a usage limit, replacing an earlier hit of the same chat.
// With quiet, nothing is pushed yet: the caller is about to add the continue and wants clients to see
// the hit and the continue together (Touch pushes if it ends up adding none).
func (s *Scheduler) SetHit(h protocol.LimitHit, quiet bool) {
	s.mu.Lock()
	s.hits[h.SessionID] = h
	s.save()
	s.mu.Unlock()
	if !quiet {
		s.changed()
	}
}

// Touch pushes the schedule to clients as it is now.
func (s *Scheduler) Touch() { s.changed() }

// Hit returns a chat's limit hit, if it has one.
func (s *Scheduler) Hit(sessionID string) (protocol.LimitHit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hits[sessionID]
	return h, ok
}

// Get returns one entry.
func (s *Scheduler) Get(id string) (protocol.ScheduledTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.findLocked(id); t != nil {
		return *t, true
	}
	return protocol.ScheduledTurn{}, false
}

// IsAuto is whether t is a continue the server armed after a usage limit.
func IsAuto(t protocol.ScheduledTurn) bool { return t.Origin.Label == protocol.OriginLabelLimit }

// AutoFor returns the continue armed for a chat that is waiting (not one already running).
func (s *Scheduler) AutoFor(sessionID string) (protocol.ScheduledTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.items {
		if t.SessionID == sessionID && IsAuto(t) && (t.Status == protocol.SchedulePending || t.Status == protocol.SchedulePaused) {
			return t, true
		}
	}
	return protocol.ScheduledTurn{}, false
}

// CountAuto is how many continues are waiting, for spreading them out when a limit resets.
func (s *Scheduler) CountAuto() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.items {
		if IsAuto(t) && (t.Status == protocol.SchedulePending || t.Status == protocol.SchedulePaused) {
			n++
		}
	}
	return n
}

// Resolve ends a chat's limit hit: the hit is forgotten and a waiting continue cancelled. Called when
// the person picks up the chat themselves, dismisses the card, or the chat is deleted. Reports
// whether there was anything to resolve.
func (s *Scheduler) Resolve(sessionID string) bool {
	s.mu.Lock()
	changed := false
	if _, ok := s.hits[sessionID]; ok {
		delete(s.hits, sessionID)
		changed = true
	}
	for i := range s.items {
		t := &s.items[i]
		if t.SessionID == sessionID && IsAuto(*t) && (t.Status == protocol.SchedulePending || t.Status == protocol.SchedulePaused) {
			t.Status = protocol.ScheduleCancelled
			changed = true
		}
	}
	if changed {
		s.save()
	}
	s.mu.Unlock()
	if changed {
		s.changed()
	}
	return changed
}

// Dir is the directory the schedule is kept in.
func (s *Scheduler) Dir() string { return filepath.Dir(s.path) }

// RememberBinding records the provider a chat runs on (saved only when it changed).
func (s *Scheduler) RememberBinding(sessionID string, b Binding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings[sessionID] == b {
		return
	}
	s.bindings[sessionID] = b
	s.save()
}

// BindingFor is the provider a chat last ran on, if known.
func (s *Scheduler) BindingFor(sessionID string) (Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[sessionID]
	return b, ok
}
