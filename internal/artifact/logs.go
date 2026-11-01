// Package artifact keeps what the pages the model wrote have logged, for the artifact_logs tool.
package artifact

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	maxEntries   = 200  // per artifact
	maxEntryLen  = 4096 // bytes
	maxArtifacts = 50   // per session
)

// Entry is one line a page logged or one error it threw.
type Entry struct {
	At   time.Time
	Kind string // "log" or "error"
	Text string
}

// Logs is a bounded in-memory ring per (session, artifact). Lost on restart: a page's logs only
// matter while the person is looking at it.
type Logs struct {
	mu      sync.Mutex
	entries map[string]map[string][]Entry
	order   map[string][]string // artifact ids of a session, oldest first
}

// Default is the server's one store.
var Default = &Logs{entries: map[string]map[string][]Entry{}, order: map[string][]string{}}

// Add records an entry, cutting it to maxEntryLen and dropping the oldest past maxEntries.
func (l *Logs) Add(sessionID, artifactID, kind, text string) {
	if len(text) > maxEntryLen {
		text = text[:maxEntryLen] + "…"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	byID := l.entries[sessionID]
	if byID == nil {
		byID = map[string][]Entry{}
		l.entries[sessionID] = byID
	}
	if _, seen := byID[artifactID]; !seen {
		l.order[sessionID] = append(l.order[sessionID], artifactID)
		if o := l.order[sessionID]; len(o) > maxArtifacts {
			delete(byID, o[0])
			l.order[sessionID] = o[1:]
		}
	}
	es := append(byID[artifactID], Entry{At: time.Now(), Kind: kind, Text: text})
	if len(es) > maxEntries {
		es = es[len(es)-maxEntries:]
	}
	byID[artifactID] = es
}

// Forget drops a deleted chat's logs.
func (l *Logs) Forget(sessionID string) {
	l.mu.Lock()
	delete(l.entries, sessionID)
	delete(l.order, sessionID)
	l.mu.Unlock()
}

// Read renders an artifact's log for the model. An empty artifactID means the session's latest
// artifact that logged anything.
func (l *Logs) Read(sessionID, artifactID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if artifactID == "" {
		if o := l.order[sessionID]; len(o) > 0 {
			artifactID = o[len(o)-1]
		}
	}
	es := l.entries[sessionID][artifactID]
	if len(es) == 0 {
		return "nothing logged: the page has not logged or thrown anything (it may not have been opened yet)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "artifact %s, %d entries (oldest first):\n", artifactID, len(es))
	for _, e := range es {
		fmt.Fprintf(&b, "%s [%s] %s\n", e.At.Format("15:04:05"), e.Kind, e.Text)
	}
	return b.String()
}
