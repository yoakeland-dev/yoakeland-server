package main

import (
	"slices"
	"yoakeland/internal/protocol"
)

// GetNotifyPrefs returns the notification policy, the defaults until the
// owner changes something.
func (s *ServerSettings) GetNotifyPrefs() protocol.NotifyPrefsData {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Notify == nil {
		return defaultNotifyPrefs()
	}
	p := *s.Notify
	p.Muted = slices.Clone(p.Muted)
	if p.Muted == nil {
		p.Muted = []string{}
	}
	return p
}

// SetNotifyPrefs stores p after sanitizing it (unknown kinds and duplicate
// mutes dropped, the threshold clamped to a day), returning what was stored.
func (s *ServerSettings) SetNotifyPrefs(p protocol.NotifyPrefsData) (protocol.NotifyPrefsData, error) {
	p.MinTurnSec = min(max(p.MinTurnSec, 0), 86400)
	muted := []string{}
	for _, k := range p.Muted {
		if _, known := kindPriority[k]; known && !slices.Contains(muted, k) {
			muted = append(muted, k)
		}
	}
	slices.Sort(muted)
	p.Muted = muted
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Notify = &p
	return p, s.save()
}
