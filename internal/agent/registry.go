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
	providerName  string
	tools         []tool.Tool
	permEngine    *permission.Engine
	snapshotStore snapshot.SnapshotStore
	sessionStore  SessionStore
	ctxMgr        *ContextManager
	usageStore    *UsageStore

	maxSessionSpendUSD float64
	webToolsEnabled    bool
	eventTap           func(protocol.EventType, string, string, any)
	hooks              Hooks
	roles              *Roles
	projectRoot        string
}

// SetHooks installs hooks on every session's Agent, current and future.
func (r *Registry) SetHooks(h Hooks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = h
	for _, ag := range r.sessions {
		ag.hooks = h
	}
}

func (r *Registry) agents() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		out = append(out, ag)
	}
	return out
}

// QueuedTurns sums every session's waiting turns.
func (r *Registry) QueuedTurns() int {
	n := 0
	for _, ag := range r.agents() {
		n += ag.QueuedTurns()
	}
	return n
}

// PendingAsks collects every session's unanswered asks.
func (r *Registry) PendingAsks() []protocol.PendingAsk {
	var out []protocol.PendingAsk
	for _, ag := range r.agents() {
		out = append(out, ag.PendingAsks()...)
	}
	return out
}

// SetEventTap installs tap on every session's event stream, current and
// future (see protocol.StreamSet.SetTap).
func (r *Registry) SetEventTap(tap func(evtType protocol.EventType, sessionID, turnID string, payload any)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventTap = tap
	for _, ag := range r.sessions {
		ag.stream.SetTap(tap)
	}
}

// ActiveTurns sums running turns across every live session.
func (r *Registry) ActiveTurns() int {
	r.mu.Lock()
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	n := 0
	for _, ag := range agents {
		n += ag.ActiveTurnCount()
	}
	return n
}

func NewRegistry(
	provider ModelProvider,
	providerName string,
	tools []tool.Tool,
	permEngine *permission.Engine,
	snapshotStore snapshot.SnapshotStore,
	sessionStore SessionStore,
	ctxMgr *ContextManager,
	usageStore *UsageStore,
	webToolsEnabled bool,
) *Registry {
	return &Registry{
		sessions:        make(map[string]*Agent),
		provider:        provider,
		providerName:    providerName,
		tools:           tools,
		permEngine:      permEngine,
		snapshotStore:   snapshotStore,
		sessionStore:    sessionStore,
		ctxMgr:          ctxMgr,
		usageStore:      usageStore,
		webToolsEnabled: webToolsEnabled,
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

// SetWebToolsEnabled applies the project-wide web_search/web_fetch toggle
// (persisted in settings.json, see cmd/yoakeland's set_web_tools_enabled
// handler) to every session created from here on, AND to every session
// already live — unlike SetMaxSessionSpendUSD, this is a live per-project
// setting a client can flip mid-session, not just a construction-time
// default.
func (r *Registry) SetWebToolsEnabled(enabled bool) {
	r.mu.Lock()
	r.webToolsEnabled = enabled
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()

	for _, ag := range agents {
		ag.SetWebToolsEnabled(enabled)
	}
}

// SetRoles installs the server-wide model role table (roles.go) on every
// session, current and future. The table itself is shared and live, so a
// later Roles.Set reaches them all without calling this again.
func (r *Registry) SetRoles(roles *Roles) {
	r.mu.Lock()
	r.roles = roles
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.mu.Lock()
		ag.roles = roles
		ag.mu.Unlock()
	}
}

// SetProjectRoot is where sessions read the project's instructions file
// from (instructions.go).
func (r *Registry) SetProjectRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projectRoot = root
}

// Get returns the Agent for sessionID, creating and loading it on first use.
func (r *Registry) Get(stream *protocol.IPCStream, sessionID string) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ag, ok := r.sessions[sessionID]; ok {
		ag.SetStream(stream) // subscribe this connection too — see Agent.SetStream
		return ag
	}

	// A fresh ContextManager per session, not the registry's shared one:
	// its MaxHistoryTokens is mutable (set_model_options' contextWindow),
	// and every session in a project shared the same *ContextManager
	// before this — one session picking a small-context model silently
	// resized every other concurrent session's context-window budget too.
	ag := NewAgent(stream, r.provider, r.providerName, r.tools, r.permEngine, r.snapshotStore, r.sessionStore, &ContextManager{
		MaxHistoryTokens:    r.ctxMgr.GetMaxHistoryTokens(),
		MaxToolOutputTokens: r.ctxMgr.MaxToolOutputTokens,
	}, r.usageStore)
	ag.SetMaxSessionSpendUSD(r.maxSessionSpendUSD)
	ag.SetWebToolsEnabled(r.webToolsEnabled)
	ag.roles, ag.projectRoot = r.roles, r.projectRoot
	if r.eventTap != nil {
		ag.stream.SetTap(r.eventTap)
	}
	ag.hooks = r.hooks
	_ = ag.LoadSession(sessionID)
	r.sessions[sessionID] = ag
	return ag
}

// Lookup returns sessionID's live Agent without creating one, or nil.
func (r *Registry) Lookup(sessionID string) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[sessionID]
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
