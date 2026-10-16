package agent

import (
	"log"
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

	limits          *Limits
	webToolsEnabled bool
	shellDisabled   bool
	defaultSource   func(projectRoot string) (ModelProvider, string) // preferred over provider (SetDefaultSource)
	eventTap        func(protocol.EventType, string, string, any)
	hooks           Hooks
	roles           *Roles
	projectRoot     string
	chat            bool
	source          tool.Source
	skillSrc        SkillSource
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
		limits:          NewLimits(0, 0),
	}
}

// SetMaxSessionSpendUSD sets the per-session spend cap in this registry's
// Limits, live for sessions already created as well as future ones. A server
// installs one shared Limits over all its registries with SetLimits instead.
func (r *Registry) SetMaxSessionSpendUSD(limit float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rounds, _ := r.limits.Get()
	r.limits.Set(rounds, limit)
}

// SetLimits shares l with every session, current and future (limits.go).
func (r *Registry) SetLimits(l *Limits) {
	if l == nil {
		l = NewLimits(0, 0)
	}
	r.mu.Lock()
	r.limits = l
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.SetLimits(l)
	}
}

// SetShellDisabled withholds bash and the terminal_* tools from every
// session, current and future (Agent.SetShellDisabled).
func (r *Registry) SetShellDisabled(disabled bool) {
	r.mu.Lock()
	r.shellDisabled = disabled
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.SetShellDisabled(disabled)
	}
}

// SetDefaultSource makes new sessions take their default provider from fn
// (falling back to the one given to NewRegistry when fn returns nil), and
// gives a live session that has no provider the current default too, so
// setting one takes effect without reopening chats.
func (r *Registry) SetDefaultSource(fn func(projectRoot string) (ModelProvider, string)) {
	r.mu.Lock()
	r.defaultSource = fn
	r.mu.Unlock()
	r.ApplyDefaultProvider()
}

// ApplyDefaultProvider re-reads the default source for sessions that still
// have no provider of their own.
func (r *Registry) ApplyDefaultProvider() {
	r.mu.Lock()
	p, name := r.defaultProviderLocked()
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	if p == nil {
		return
	}
	for _, ag := range agents {
		ag.mu.Lock()
		if ag.provider == nil {
			ag.provider, ag.providerName = p, name
		}
		ag.mu.Unlock()
	}
}

func (r *Registry) defaultProviderLocked() (ModelProvider, string) {
	if r.defaultSource != nil {
		if p, name := r.defaultSource(r.projectRoot); p != nil {
			return p, name
		}
	}
	return r.provider, r.providerName
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

// SetChat marks this registry's sessions as project-less chats.
func (r *Registry) SetChat(chat bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chat = chat
}

// IsChat reports SetChat's value.
func (r *Registry) IsChat() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chat
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
	defProv, defName := r.defaultProviderLocked()
	ag := NewAgent(stream, defProv, defName, r.tools, r.permEngine, r.snapshotStore, r.sessionStore, &ContextManager{
		MaxHistoryTokens:    r.ctxMgr.GetMaxHistoryTokens(),
		MaxToolOutputTokens: r.ctxMgr.MaxToolOutputTokens,
	}, r.usageStore)
	ag.SetLimits(r.limits)
	ag.SetWebToolsEnabled(r.webToolsEnabled)
	ag.SetShellDisabled(r.shellDisabled)
	ag.roles, ag.projectRoot, ag.chat = r.roles, r.projectRoot, r.chat
	ag.source, ag.skillSrc = r.source, r.skillSrc
	if r.eventTap != nil {
		ag.stream.SetTap(r.eventTap)
	}
	ag.hooks = r.hooks
	if err := ag.LoadSession(sessionID); err != nil {
		// Not kept, so the next Get reads the file again. This agent refuses
		// its turns with the error, so nothing runs on the empty chat it shows.
		log.Printf("session %s: %v", sessionID, err)
		return ag
	}
	r.sessions[sessionID] = ag
	return ag
}

// SessionReport is the context meter and running spend of sessionID as
// turn_completed would report them for history, for a client that has just
// opened the session or switched branch. The context is nil for an empty
// history, the spend nil without a usage store.
func (r *Registry) SessionReport(sessionID string, history []Message) (*protocol.ContextData, *protocol.SpendData) {
	var ctx *protocol.ContextData
	if len(history) > 0 {
		cm, model, overhead := r.ctxMgr, "", 0
		if ag := r.Lookup(sessionID); ag != nil {
			cm, model, overhead = ag.contextManager, ag.getModelName(), ag.requestOverhead()
		}
		c := cm.Report(history, model, overhead)
		ctx = &c
	}
	var spend *protocol.SpendData
	if r.usageStore != nil {
		sess, proj := r.usageStore.SessionTotal(sessionID), r.usageStore.ProjectTotal()
		spend = &protocol.SpendData{
			SessionTotalUSD: sess.CostUSD, ProjectTotalUSD: proj.CostUSD,
			SessionCostUnknown: sess.CostUnknown, ProjectCostUnknown: proj.CostUnknown,
		}
	}
	return ctx, spend
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
