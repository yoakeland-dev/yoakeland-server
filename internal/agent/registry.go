package agent

import (
	"sync"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// Registry hands out one Agent per session, so concurrent sessions run
// independently instead of sharing (and corrupting) a single Agent's history.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*Agent

	provider      ModelProvider
	tools         []tool.Tool
	permEngine    *permission.Engine
	snapshotStore snapshot.SnapshotStore
	sessionStore  SessionStore
	ctxMgr        *ContextManager
	usageStore    *UsageStore

	maxSessionSpendUSD float64
}

func NewRegistry(
	provider ModelProvider,
	tools []tool.Tool,
	permEngine *permission.Engine,
	snapshotStore snapshot.SnapshotStore,
	sessionStore SessionStore,
	ctxMgr *ContextManager,
	usageStore *UsageStore,
) *Registry {
	return &Registry{
		sessions:      make(map[string]*Agent),
		provider:      provider,
		tools:         tools,
		permEngine:    permEngine,
		snapshotStore: snapshotStore,
		sessionStore:  sessionStore,
		ctxMgr:        ctxMgr,
		usageStore:    usageStore,
	}
}

// SetMaxSessionSpendUSD applies a per-session spend cap (see
// Agent.SetMaxSessionSpendUSD) to every session created from here on.
// Sessions already created keep whatever cap they had.
func (r *Registry) SetMaxSessionSpendUSD(limit float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxSessionSpendUSD = limit
}

// Get returns the Agent for sessionID, creating and loading it on first use.
func (r *Registry) Get(stream *protocol.IPCStream, sessionID string) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ag, ok := r.sessions[sessionID]; ok {
		ag.SetStream(stream) // rebind to latest connection (last writer wins on reconnect)
		return ag
	}

	ag := NewAgent(stream, r.provider, r.tools, r.permEngine, r.snapshotStore, r.sessionStore, r.ctxMgr, r.usageStore)
	ag.SetMaxSessionSpendUSD(r.maxSessionSpendUSD)
	_ = ag.LoadSession(sessionID)
	r.sessions[sessionID] = ag
	return ag
}

// Remove evicts sessionID's in-memory Agent, if one was ever created,
// cancelling any turn it has in flight first. Callers still own deleting the
// persisted session file separately (SessionStore.Delete) — this only drops
// the live handle so a stale Agent can't keep running or get re-fetched by
// Get after the underlying session is gone. Discard (before the cancel) is
// what stops that turn's own final save from writing the file back after the
// caller removes it.
func (r *Registry) Remove(sessionID string) {
	r.mu.Lock()
	ag, ok := r.sessions[sessionID]
	delete(r.sessions, sessionID)
	r.mu.Unlock()

	if ok {
		ag.Discard()
		ag.CancelAll()
	}
}
