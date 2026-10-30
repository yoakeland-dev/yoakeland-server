package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/audit"
	"yoakeland/internal/mcp"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/settings"
	"yoakeland/internal/skills"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/terminal"
	"yoakeland/internal/tool"
)

// ProjectContext bundles everything scoped to one project root: its own
// sandbox boundary (normalizer + sandbox), permission engine, snapshot and
// session stores, and its own session registry. Two projects never share
// any of this — a tool call in one project's sandbox can't touch another
// project's files, and one project's sessions are invisible to another's.
type ProjectContext struct {
	Root          string
	SnapStore     snapshot.SnapshotStore
	SessStore     agent.SessionStore
	Registry      *agent.Registry
	SandboxTier   string
	SettingsStore *settings.Store
	Terminals     *terminal.Manager
	// Permissions is the rule engine the project's agents consult; the
	// permission-rule requests read and edit it.
	Permissions *permission.Engine
	// MCP runs the project's MCP servers; nil when MCP could not be opened.
	MCP *mcp.Manager
}

// chatReader lets the read_chat tool read the project's other sessions.
type chatReader struct{ store agent.SessionStore }

func (r chatReader) ReadChat(id string) (string, []tool.ChatTurn, error) {
	st, err := r.store.Load(id)
	if err != nil {
		return "", nil, err
	}
	var turns []tool.ChatTurn
	for _, m := range st.History {
		if m.Role == "user" || m.Role == "assistant" {
			turns = append(turns, tool.ChatTurn{Role: m.Role, Text: m.Content})
		}
	}
	return st.Title, turns, nil
}

func newProjectContext(root, dataDir string, defaultProvider agent.ModelProvider, defaultProviderName string, limits *agent.Limits, disableBashUnsandboxed bool, browser tool.BrowserRelay, skillCat *skills.Catalog) (*ProjectContext, error) {
	norm, err := normalizer.New(root)
	if err != nil {
		return nil, fmt.Errorf("normalizer: %w", err)
	}

	// The folder name is a hash; this says which project it is.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("project data dir: %w", err)
	}
	meta, _ := json.Marshal(map[string]string{"path": norm.ProjectRoot})
	_ = os.WriteFile(filepath.Join(dataDir, "project.json"), meta, 0o600)

	permStore := permission.NewPersistentStore(dataDir)
	permEngine := permission.New(norm, permStore)

	settingsStore := settings.NewStore(dataDir)
	projSettings, err := settingsStore.Load()
	if err != nil {
		return nil, fmt.Errorf("settings store: %w", err)
	}

	snapStore, err := snapshot.NewDiskSnapshotStore(dataDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}

	sessStore, err := agent.NewDiskSessionStore(dataDir)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}

	sb := sandbox.NewSandbox(norm.ProjectRoot, 60*time.Second)
	if sb.Mode() == sandbox.ModeLocalRestr {
		logWarnDetail(
			fmt.Sprintf("no sandbox for project %q; bash tool runs unconfined", norm.ProjectRoot),
			fmt.Sprintf("no sandbox available for project %q (bubblewrap and Linux namespaces both unavailable): the bash tool has no process or filesystem isolation beyond the project-root path check, so a command can see and touch anything this OS user can. Install bubblewrap (bwrap) for isolation.", norm.ProjectRoot),
		)
	}
	// The read-only presets (ls, git status...) are only trusted where bash
	// really is confined by bubblewrap.
	permEngine.SetPresets(sb.Mode() == sandbox.ModeBubblewrap)
	// Reuses the same sb this project's bash tool runs through (see the
	// sandbox-tier warning above) — a remote peer's interactive terminal
	// gets exactly the isolation an agent-issued command does, not more.
	terminals := terminal.NewManager(sb, norm)
	terminals.SetAgentNetwork(projSettings.WebToolsEnabled)
	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewSearchTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
		tool.NewAskUserQuestionTool(),
		tool.NewWritePlanTool(filepath.Join(dataDir, "plans")),
		tool.NewTerminalStartTool(terminals),
		tool.NewTerminalOutputTool(terminals),
		tool.NewTerminalStopTool(terminals),
		tool.NewBrowserTool(browser),
		tool.NewReadChatTool(chatReader{sessStore}),
	}
	braveKey := os.Getenv("BRAVE_API_KEY")
	if braveKey == "" {
		logWarnDetail(
			"web_search tool using DuckDuckGo fallback: no BRAVE_API_KEY",
			"BRAVE_API_KEY is not set, so the web_search tool falls back to scraping DuckDuckGo, which has no setup cost but can intermittently fail bot checks. Get a key at https://brave.com/search/api/ and set BRAVE_API_KEY for reliable search.",
		)
	}
	tools = append(tools, tool.NewWebSearchTool(braveKey), tool.NewWebFetchTool())
	var projSkills agent.SkillSource
	if skillCat != nil {
		ps := projectSkills{cat: skillCat, root: norm.ProjectRoot}
		tools = append(tools, tool.NewSkillTool(ps))
		projSkills = ps
	}

	ctxMgr := agent.NewContextManager(64000, 4000)
	usageStore, err := agent.NewUsageStore(dataDir)
	if err != nil {
		return nil, fmt.Errorf("usage store: %w", err)
	}

	registry := agent.NewRegistry(defaultProvider, defaultProviderName, tools, permEngine, snapStore, sessStore, ctxMgr, usageStore, projSettings.WebToolsEnabled)
	registry.SetLimits(limits)
	registry.SetProjectRoot(norm.ProjectRoot)
	if projSkills != nil {
		registry.SetSkills(projSkills)
	}

	pc := &ProjectContext{
		Root:          norm.ProjectRoot,
		SnapStore:     snapStore,
		SessStore:     sessStore,
		Registry:      registry,
		SandboxTier:   string(sb.Mode()),
		SettingsStore: settingsStore,
		Terminals:     terminals,
		Permissions:   permEngine,
	}
	pc.applyShellPolicy(disableBashUnsandboxed)
	return pc, nil
}

// applyShellPolicy withholds bash and the terminal_* tools from this
// project's sessions when the server's disableBashWhenUnsandboxed setting is
// on and the project has no real sandbox (tier local_restricted_fallback).
// Any real tier keeps them, whatever the setting. A remote peer's own
// interactive terminal is not affected: that is the user, not the model.
func (pc *ProjectContext) applyShellPolicy(disableWhenUnsandboxed bool) {
	pc.Registry.SetShellDisabled(disableWhenUnsandboxed && pc.SandboxTier == string(sandbox.ModeLocalRestr))
}

// toHistoryMessages converts a session's stored agent.Message history into
// the wire shape for get_session_history's response. agent.Message can't be
// used directly on the wire since internal/protocol can't import
// internal/agent (agent already imports protocol). branches is the same
// session's stashed alternates (SessionState.Branches, possibly nil) — used
// to compute each message's AlternateIDs at read time rather than storing a
// second, derivable copy of that on agent.Message itself (nothing to drift
// out of sync with Branches that way).
func toHistoryMessages(history []agent.Message, branches map[string][][]agent.Message) []protocol.HistoryMessage {
	out := make([]protocol.HistoryMessage, 0, len(history))
	for i, m := range history {
		// A subagent call's transcript goes just before its result, each
		// message tagged with the call it belongs to.
		for _, sm := range toHistoryMessages(m.Subagent, nil) {
			sm.ParentToolCallID = m.ToolCallID
			out = append(out, sm)
		}
		branchPoint := ""
		if i > 0 {
			branchPoint = history[i-1].ID
		}
		hm := protocol.HistoryMessage{
			ID:           m.ID,
			Role:         m.Role,
			Content:      m.Content,
			Reasoning:    m.Reasoning,
			ToolCallID:   m.ToolCallID,
			Diffs:        m.Diffs,
			Images:       m.Images,
			AlternateIDs: alternateIDs(branches[branchPoint]),
			Author:       m.AuthorName,
			AuthorAvatar: m.AuthorAvatar,
			Attachments:  m.Attachments,
			Provider:     m.Provider,
			Model:        m.Model,
			CreatedAt:    m.CreatedAt,
			GenerationMs: m.GenerationMs,
			Usage:        m.Usage,
			Summary:      m.Summary,
			Ask:          m.Ask,
			Actions:      m.Actions,
			Thread:       m.Thread,
		}
		if c := m.Compaction; c != nil {
			hm.Compaction = &protocol.CompactionInfo{Kind: c.Kind, TokensBefore: c.TokensBefore, TokensAfter: c.TokensAfter}
		}
		for _, tc := range m.ToolCalls {
			hm.ToolCalls = append(hm.ToolCalls, protocol.HistoryToolCall{
				ID:             tc.ID,
				Name:           tc.Name,
				Args:           tc.Args,
				NativeToolInfo: tc.NativeToolInfo,
			})
		}
		out = append(out, hm)
	}
	return out
}

// alternateIDs returns the head-message ID of each stashed alternate at one
// branch point — the other versions a client can switch_branch to. nil (and
// so omitted on the wire) when there are none.
func alternateIDs(alts [][]agent.Message) []string {
	if len(alts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(alts))
	for _, alt := range alts {
		if len(alt) > 0 {
			ids = append(ids, alt[0].ID)
		}
	}
	return ids
}

// sessionSummaryFrom builds the wire summary shared by session_list and
// session_updated from a persisted session's state.
func sessionSummaryFrom(root string, state *agent.SessionState) protocol.SessionSummary {
	return protocol.SessionSummary{
		SessionID:       state.SessionID,
		ProjectPath:     root,
		UpdatedAt:       state.UpdatedAt,
		MessageCount:    len(state.History),
		Title:           state.Title,
		Archived:        state.Archived,
		LinkedSessionID: state.LinkedSessionID,
		Mode:            state.Mode,
	}
}

// listFilesIgnoredDirs are skipped outright during listProjectFiles, on top
// of anything dot-prefixed (.git, .yoake, .venv, .svelte-kit, ...).
var listFilesIgnoredDirs = map[string]bool{
	"node_modules": true,
	"target":       true,
	"dist":         true,
	"build":        true,
	"vendor":       true,
	"__pycache__":  true,
}

// listFilesMax caps how many paths listProjectFiles returns. The wire
// framing (SPEC.md §2) caps a single line at 10MB; a cap here keeps a huge
// monorepo's file list from ever getting close to that rather than failing
// client-side mid-stream.
const listFilesMax = 20000

// listProjectFiles walks root and returns every regular file's path
// relative to it (forward-slash separated, sorted), for a client's @file
// mention autocomplete. Skips VCS/dependency/build directories and anything
// dot-prefixed — none of that is ever something a prompt should reference.
func listProjectFiles(root string) ([]string, error) {
	files := make([]string, 0, 256)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || listFilesIgnoredDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if len(files) >= listFilesMax {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	return files, err
}

// listDirectory returns path's immediate entries (dirs first, then
// alphabetical), for a project-picker browsing the server's own filesystem
// rather than the client's. No dotfile/VCS filtering — this walks toward a
// project root, it isn't listing one.
func listDirectory(path string) (string, []protocol.DirEntry, error) {
	if path == "" || path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolving home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolving path %q: %w", path, err)
	}
	raw, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, err
	}
	entries := make([]protocol.DirEntry, 0, len(raw))
	for _, e := range raw {
		entries = append(entries, protocol.DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return abs, entries, nil
}

// ProjectRegistry hands out one ProjectContext per project root, creating
// it lazily on first use, and tracks which project each session belongs to
// so later envelopes referencing only a sessionId (not a path) resolve to
// the right one. This is what makes one server process able to serve many
// projects at once, opened on demand as clients ask for them.
type ProjectRegistry struct {
	mu                  sync.Mutex
	projects            map[string]*ProjectContext // key: canonicalized project root
	sessionProject      map[string]string          // sessionID -> project key
	defaultProvider     agent.ModelProvider
	defaultProviderName string
	// limits is the one soft round/spend limit holder every project's sessions
	// read live; disableBashUnsandboxed is the runtime setting applied to each
	// project by its sandbox tier. Both are written by ApplyRuntime.
	limits                 *agent.Limits
	disableBashUnsandboxed bool
	defaultSource          func(root string) (agent.ModelProvider, string)
	known                  *projectstore.Store // persisted "which projects exist", server-wide
	stateDir               string
	// chatsDir (canonical) holds one scratch root per project-less chat;
	// chatTier is the sandbox tier they get.
	chatsDir string
	chatTier string
	notify   *NotificationFeed
	audit    *audit.Log
	hub      *statusHub
	// gits watches every known project's git state, open or not, so a
	// client sees all its repos (SPEC.md §3.2 git_status). A nil entry is
	// a known non-repo, so it isn't re-checked on every open.
	// ponytail: one 1s mtime poll per known project; fine for dozens, move
	// to fsnotify if people keep hundreds of projects around.
	gits map[string]*gitWatcher
	// browser relays every project's browser tool (nil: none, in tests).
	browser *browserBroker
	// browserMCP hands the same browser to CLI agents as an MCP server (browser_mcp.go).
	browserMCP *browserMCP
	// roles is the server-wide model role table every session shares.
	roles *agent.Roles
	// ext is the MCP servers and skills every project shares (extensions.go).
	ext *extensions
}

// SetNotifications routes every project's session events (current and
// future) through feed's sessionTap.
func (r *ProjectRegistry) SetNotifications(feed *NotificationFeed) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = feed
	for _, pc := range r.projects {
		r.installLocked(pc)
	}
}

// SetAudit routes every project's (current and future) audit events, tagged
// with their project, into log.
func (r *ProjectRegistry) SetAudit(log *audit.Log) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audit = log
	for _, pc := range r.projects {
		r.installLocked(pc)
	}
}

// SetStatusHub routes every project's (current and future) turn
// completions, queue/ask changes, and git state through hub.
func (r *ProjectRegistry) SetStatusHub(hub *statusHub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hub = hub
	hub.asks = r.PendingAsks
	hub.terminals = r.Terminals
	for _, pc := range r.projects {
		r.installLocked(pc)
	}
	go func() {
		for _, root := range r.List() {
			r.ensureGit(root)
		}
	}()
}

// ensureGit starts root's git watcher if it has none yet. It runs git, so
// it's called off r.mu.
func (r *ProjectRegistry) ensureGit(root string) {
	r.mu.Lock()
	_, ok := r.gits[root]
	hub := r.hub
	r.mu.Unlock()
	if ok || hub == nil || r.isChat(root) {
		return
	}
	w := startGitWatcher(root, hub.BroadcastGit)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.gits[root]; ok {
		w.Stop() // lost a race with another ensureGit
		return
	}
	r.gits[root] = w
}

func (r *ProjectRegistry) git(root string) *gitWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gits[root]
}

// GitStatus is root's last git state, or nil (not a repo, or not read yet).
func (r *ProjectRegistry) GitStatus(root string) *protocol.GitStatusData {
	return r.git(root).Last()
}

// GitStatuses is every known repo's last git state.
func (r *ProjectRegistry) GitStatuses() []protocol.GitStatusData {
	r.mu.Lock()
	ws := make([]*gitWatcher, 0, len(r.gits))
	for _, w := range r.gits {
		ws = append(ws, w)
	}
	r.mu.Unlock()
	var out []protocol.GitStatusData
	for _, w := range ws {
		if st := w.Last(); st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// Terminals lists every terminal in every open project.
func (r *ProjectRegistry) Terminals() []protocol.TerminalSummary {
	out := []protocol.TerminalSummary{}
	for _, pc := range r.open() {
		for _, t := range pc.Terminals.All() {
			t.ProjectPath = pc.Root
			out = append(out, t)
		}
	}
	return out
}

// installLocked (r.mu held) wires pc's sessions to the notification feed
// and status hub, whichever are set. Safe to repeat.
func (r *ProjectRegistry) installLocked(pc *ProjectContext) {
	hub := r.hub
	var hooks agent.Hooks
	if log := r.audit; log != nil {
		root := pc.Root
		hooks.Audit = func(e audit.Event) {
			e.Project = root
			log.Append(e)
		}
	}
	if hub != nil {
		asksChanged := hub.BroadcastPendingAsks
		if feed := r.notify; feed != nil {
			asksChanged = func() {
				hub.BroadcastPendingAsks()
				feed.Reconcile(r.PendingAsks(), r.sessionTitle)
			}
		}
		hooks.QueueChanged, hooks.AsksChanged = hub.PushStatus, asksChanged
		pc.Terminals.SetOnChange(hub.BroadcastTerminals)
		go r.ensureGit(pc.Root)
	}
	pc.Registry.SetHooks(hooks)
	var notifyTap func(protocol.EventType, string, string, any)
	if r.notify != nil {
		notifyTap = r.notify.sessionTap(pc.Root, pc.SessStore)
	}
	root := pc.Root
	pc.Registry.SetEventTap(func(evt protocol.EventType, sessionID, turnID string, payload any) {
		if notifyTap != nil {
			notifyTap(evt, sessionID, turnID, payload)
		}
		if evt == protocol.EventTurnCompleted {
			if d, ok := payload.(protocol.TurnCompletedData); ok {
				hub.TurnCompleted(d)
			}
			r.git(root).Poke() // a turn's writes don't touch .git, so the mtime poll can't see them
		}
	})
}

// sessionTitle names a chat for a notification body.
func (r *ProjectRegistry) sessionTitle(root, sessionID string) string {
	r.mu.Lock()
	pc := r.projects[root]
	r.mu.Unlock()
	if pc == nil {
		return "Untitled chat"
	}
	return sessionLabel(pc.SessStore, sessionID)
}

// QueuedTurns sums waiting turns across every open project.
func (r *ProjectRegistry) QueuedTurns() int {
	n := 0
	for _, pc := range r.open() {
		n += pc.Registry.QueuedTurns()
	}
	return n
}

// PendingAsks collects every open project's unanswered asks, oldest first.
func (r *ProjectRegistry) PendingAsks() []protocol.PendingAsk {
	var out []protocol.PendingAsk
	for _, pc := range r.open() {
		for _, ask := range pc.Registry.PendingAsks() {
			ask.ProjectPath = pc.Root
			out = append(out, ask)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (r *ProjectRegistry) open() []*ProjectContext {
	r.mu.Lock()
	defer r.mu.Unlock()
	pcs := make([]*ProjectContext, 0, len(r.projects))
	for _, pc := range r.projects {
		pcs = append(pcs, pc)
	}
	return pcs
}

// Stats reports running turns across every open project, and how many
// projects are open, for the heartbeat's server status.
func (r *ProjectRegistry) Stats() (activeTurns, openProjects int) {
	r.mu.Lock()
	pcs := make([]*ProjectContext, 0, len(r.projects))
	for _, pc := range r.projects {
		pcs = append(pcs, pc)
	}
	r.mu.Unlock()
	for _, pc := range pcs {
		activeTurns += pc.Registry.ActiveTurns()
	}
	return activeTurns, len(pcs)
}

// NewProjectRegistry builds a project registry backed by a persisted list of
// known projects under stateDir (<-state-dir>/projects.json, alongside
// providers.json) — this is what makes list_projects reflect every project
// this server has ever opened, not just ones opened in the current process's
// lifetime.
func NewProjectRegistry(defaultProvider agent.ModelProvider, defaultProviderName string, maxSessionSpendUSD float64, stateDir, masterKey string) (*ProjectRegistry, error) {
	known, err := projectstore.New(stateDir)
	if err != nil {
		return nil, fmt.Errorf("project list store: %w", err)
	}
	chatsDir := filepath.Join(stateDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o700); err != nil {
		return nil, fmt.Errorf("chats dir: %w", err)
	}
	if chatsDir, err = projectKey(chatsDir); err != nil {
		return nil, fmt.Errorf("chats dir: %w", err)
	}
	pruneChats(stateDir, chatsDir)
	return &ProjectRegistry{
		chatsDir:            chatsDir,
		chatTier:            string(sandbox.NewSandbox(chatsDir, time.Second).Mode()),
		projects:            make(map[string]*ProjectContext),
		sessionProject:      make(map[string]string),
		defaultProvider:     defaultProvider,
		defaultProviderName: defaultProviderName,
		limits:              agent.NewLimits(0, maxSessionSpendUSD),
		known:               known,
		stateDir:            stateDir,
		gits:                make(map[string]*gitWatcher),
		roles:               agent.NewRoles(),
		ext:                 newExtensions(stateDir, masterKey),
	}, nil
}

// ApplyRuntime pushes the runtime settings this registry consumes (the soft
// round and spend limits, and the bash-when-unsandboxed policy) into every
// open project and remembers them for projects opened later. It is the
// ServerSettings.OnRuntimeChange hook for those settings.
func (r *ProjectRegistry) ApplyRuntime(rs protocol.RuntimeSettings) {
	r.limits.Set(rs.MaxRoundsPerTurn, rs.MaxSessionCostUSD)
	r.browser.SetBlockCrossOrigin(rs.BrowserBlockCrossOrigin)
	r.mu.Lock()
	r.disableBashUnsandboxed = rs.DisableBashWhenUnsandboxed
	pcs := make([]*ProjectContext, 0, len(r.projects))
	for _, pc := range r.projects {
		pcs = append(pcs, pc)
	}
	r.mu.Unlock()
	for _, pc := range pcs {
		pc.applyShellPolicy(rs.DisableBashWhenUnsandboxed)
	}
}

// SetDefaultSource makes every project's new (and provider-less) sessions
// take their default provider from fn: the server's persisted
// default_provider setting. Reapply after that setting changes.
func (r *ProjectRegistry) SetDefaultSource(fn func(root string) (agent.ModelProvider, string)) {
	r.mu.Lock()
	r.defaultSource = fn
	pcs := make([]*ProjectContext, 0, len(r.projects))
	for _, pc := range r.projects {
		pcs = append(pcs, pc)
	}
	r.mu.Unlock()
	for _, pc := range pcs {
		pc.Registry.SetDefaultSource(fn)
	}
}

// ApplyDefaultProvider gives every open project's provider-less sessions the
// current default provider (after set_default_provider).
func (r *ProjectRegistry) ApplyDefaultProvider() {
	r.mu.Lock()
	pcs := make([]*ProjectContext, 0, len(r.projects))
	for _, pc := range r.projects {
		pcs = append(pcs, pc)
	}
	r.mu.Unlock()
	for _, pc := range pcs {
		pc.Registry.ApplyDefaultProvider()
	}
}

// projectKey canonicalizes root the same way normalizer.New does (symlinks
// resolved, not just made absolute) so the registry's map key always agrees
// with ProjectContext.Root — otherwise a symlinked path and its real-path
// equivalent silently become two independent ProjectContexts pointed at
// different data dirs for the same project.
func projectKey(root string) (string, error) {
	return normalizer.CanonicalRoot(root)
}

// Get returns the ProjectContext for root, opening (creating on-disk state
// for) it on first use. Safe to call repeatedly for the same root.
func (r *ProjectRegistry) Get(root string) (*ProjectContext, error) {
	key, err := projectKey(root)
	if err != nil {
		return nil, fmt.Errorf("resolving project path %q: %w", root, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if pc, ok := r.projects[key]; ok {
		return pc, nil
	}

	var browser tool.BrowserRelay
	if r.browser != nil {
		browser = r.browser
		if r.browserMCP == nil {
			r.browserMCP = newBrowserMCP(r.browser)
		}
	}
	pc, err := newProjectContext(key, projectstore.DataDir(r.stateDir, key), r.defaultProvider, r.defaultProviderName, r.limits, r.disableBashUnsandboxed, browser, r.ext.skills)
	if err != nil {
		return nil, err
	}
	if r.ext.mcpStore != nil {
		var notifier mcpNotifier
		pc.MCP = mcp.NewManager(key, r.ext.mcpStore, r.ext.mcpAuth, r.ext.mcpFlow, func() { notifier.poke(func() { r.broadcastMCP(pc) }) })
		pc.Registry.SetToolSource(pc.MCP)
	}
	if r.browserMCP != nil {
		pc.Registry.SetNativeServers(r.browserMCP.serversFor)
	}
	pc.Permissions.SetGlobal(permission.OpenGlobal(r.stateDir))
	r.projects[key] = pc
	r.installLocked(pc)
	pc.Registry.SetChat(r.isChat(key))
	pc.Registry.SetRoles(r.roles)
	pc.Registry.SetDefaultSource(r.defaultSource)

	// Only recorded on an actual fresh open (not the map-hit fast path
	// above), so a hot session doesn't turn every envelope it touches into
	// a projects.json rewrite. ponytail: LastOpenedAt only advances once
	// per process-lifetime per project, not per request — fine for a
	// "recent projects" sort, not meant as a precise last-touched clock.
	if r.isChat(key) {
		return pc, nil // a chat's scratch root isn't a project to list
	}
	if err := r.known.Upsert(key); err != nil {
		logWarn("failed to record project %q in projects.json: %v", key, err)
	}
	return pc, nil
}

// Unbind removes sessionID's project binding, e.g. after the session is
// deleted, so the id can be rebound to a different project later instead of
// leaking in sessionProject forever.
func (r *ProjectRegistry) Unbind(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessionProject, sessionID)
}

// Remove forgets root: drops it from the known-projects list (projects.json)
// so it stops being offered back to clients via List/project_list, and tears
// down its live ProjectContext if one exists — cancelling every session
// bound to it (via agent.Registry.Remove, the same helper delete_session
// already uses) before dropping the ProjectContext itself. This is what
// closes the "session outlives its project" gap: without it, a session still
// bound to a removed project would silently rebuild a brand-new
// ProjectContext (new sandbox, new agent.Registry) on its next reference
// while the old one — possibly mid-turn — kept running, unreferenced but
// alive. Its data dir stays, so re-adding the project finds its history.
func (r *ProjectRegistry) Remove(root string) error {
	key, err := projectKey(root)
	if err != nil {
		return fmt.Errorf("resolving project path %q: %w", root, err)
	}

	r.mu.Lock()
	pc, ok := r.projects[key]
	delete(r.projects, key)
	var boundSessions []string
	for sessID, projKey := range r.sessionProject {
		if projKey == key {
			boundSessions = append(boundSessions, sessID)
			delete(r.sessionProject, sessID)
		}
	}
	r.mu.Unlock()

	if ok {
		for _, sessID := range boundSessions {
			pc.Registry.Remove(sessID)
			pc.Terminals.CloseSession(sessID)
			provider.CloseNativeSessions(agent.NativeSessionKey(sessID, pc.Root))
		}
	}
	r.mu.Lock()
	r.gits[key].Stop()
	delete(r.gits, key)
	r.mu.Unlock()
	if ok && pc.MCP != nil {
		pc.MCP.Close()
	}
	if r.ext.mcpStore != nil {
		r.ext.mcpStore.ForgetProject(key)
	}

	return r.known.Remove(key)
}

// broadcastMCP pushes a project's MCP server status to every peer, for a server
// that finished connecting, failed, or changed its tools.
func (r *ProjectRegistry) broadcastMCP(pc *ProjectContext) {
	r.mu.Lock()
	hub := r.hub
	r.mu.Unlock()
	if hub == nil || hub.peers == nil {
		return
	}
	hub.peers.BroadcastEvent("", protocol.EventMCPServers, "", mcpPayload(pc))
}

// CloseMCP stops every project's MCP servers (at shutdown).
func (r *ProjectRegistry) CloseMCP() {
	for _, pc := range r.open() {
		if pc.MCP != nil {
			pc.MCP.Close()
		}
	}
}

// List returns every project this server has ever opened, most-recently-used
// first — backed by projects.json, so it survives a restart. This is no
// longer a snapshot of "opened this run"; see projectstore.Store.
func (r *ProjectRegistry) List() []string {
	known, err := r.known.List()
	if err != nil {
		logWarn("failed to read projects.json: %v", err)
		return nil
	}
	out := make([]string, 0, len(known))
	for _, p := range known {
		out = append(out, p.Path)
	}
	return out
}

// Known reports whether key (a projectKey) is a project this server has
// already opened, in this run or an earlier one.
func (r *ProjectRegistry) Known(key string) bool {
	r.mu.Lock()
	_, open := r.projects[key]
	r.mu.Unlock()
	return open || r.isChat(key) || slices.Contains(r.List(), key)
}

// Within reports whether dir is a known project's root or inside one, with symlinks resolved.
// A remote connection browses a project's files this way, not the rest of the host's.
func (r *ProjectRegistry) Within(dir string) bool {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(real) {
		return false
	}
	for _, root := range r.List() {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// scopeRemoteProject keeps a TCP peer to projects the server already knows
// (opened from the local socket, via -project, or in an earlier run). Without
// it a token reached any directory: open_project "/home/you" then read_file
// ".ssh/id_ed25519". The path is replaced by its canonical form so a symlink
// flipped after this check can't send the handler somewhere else. Every
// case-variant of the key is checked, since encoding/json matches field
// names case-insensitively.
// ponytail: a known project nested inside another can still be swapped for a
// symlink from the outer one's sandbox between here and the handler; closing
// it means fd-based (openat/RESOLVE_BENEATH) path handling in every handler.
func scopeRemoteProject(deps connDeps, env *protocol.Envelope) error {
	field := "projectPath"
	if env.Type == protocol.EventOpenProject || env.Type == protocol.EventRemoveProject {
		field = "path"
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(env.Data, &m) != nil {
		return nil // not an object; the handler reports it as malformed
	}
	changed := false
	for k, raw := range m {
		if !strings.EqualFold(k, field) {
			continue
		}
		var p string
		if json.Unmarshal(raw, &p) != nil || p == "" || p == protocol.ChatProject {
			continue
		}
		key, err := projectKey(p)
		if err != nil || !deps.projects.Known(key) {
			return fmt.Errorf("project %q isn't open on this server; remote connections can only use projects opened here", p)
		}
		m[k], _ = json.Marshal(key)
		changed = true
	}
	if changed {
		env.Data, _ = json.Marshal(m)
	}
	return nil
}

// Resolve figures out which project a session-scoped envelope refers to:
//   - an explicit path wins, and binds sessionID to it going forward
//   - a path conflicting with the session's existing binding is an error —
//     a session's project is fixed for its lifetime, not reassignable
//   - otherwise fall back to the session's existing binding
//   - otherwise fall back to defaultRoot (the server's -project, if any)
//   - otherwise it's an error: nothing to resolve against
func (r *ProjectRegistry) Resolve(sessionID, explicitPath, defaultRoot string) (*ProjectContext, error) {
	if explicitPath != "" {
		var key string
		var err error
		if explicitPath == protocol.ChatProject {
			key, err = r.chatRoot(sessionID)
		} else {
			key, err = projectKey(explicitPath)
		}
		if err != nil {
			return nil, fmt.Errorf("resolving project path %q: %w", explicitPath, err)
		}

		// An envelope with no session (set_web_tools_enabled names only a project) must not bind
		// the empty id, or the next project to name no session would hit this conflict.
		r.mu.Lock()
		if sessionID != "" {
			if bound, ok := r.sessionProject[sessionID]; ok && bound != key {
				r.mu.Unlock()
				return nil, fmt.Errorf("session %s is already bound to project %q, not %q", sessionID, bound, key)
			}
			r.sessionProject[sessionID] = key
		}
		r.mu.Unlock()

		return r.Get(key)
	}

	r.mu.Lock()
	bound, ok := r.sessionProject[sessionID]
	r.mu.Unlock()
	if ok {
		return r.Get(bound)
	}

	if defaultRoot != "" {
		// Not recorded as a binding: an event that merely falls back to the
		// server's default project (no explicit projectPath given) must not
		// permanently lock the session to it — only an explicit projectPath
		// does that, above. Otherwise the first stray event for a session
		// (e.g. a permission response) would bind it to defaultRoot before
		// its real project is ever named, and every later multi-project use
		// of that session id would fail with "already bound".
		key, err := projectKey(defaultRoot)
		if err != nil {
			return nil, fmt.Errorf("resolving default project path %q: %w", defaultRoot, err)
		}
		return r.Get(key)
	}

	return nil, fmt.Errorf("session %s has no project bound and no projectPath given", sessionID)
}

var chatIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// isChat reports whether key is a project-less chat's scratch root.
func (r *ProjectRegistry) isChat(key string) bool {
	return filepath.Dir(key) == r.chatsDir
}

// chatRoot creates (if needed) and returns the scratch root of a project-less
// chat: <state-dir>/chats/<sessionID>, an ordinary directory the session's
// sandbox, terminals and tools treat as a project root.
func (r *ProjectRegistry) chatRoot(sessionID string) (string, error) {
	if !chatIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("a chat needs a sessionId of letters, digits, '-' and '_'")
	}
	dir := filepath.Join(r.chatsDir, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ListChats is every project-less chat's session, read straight from disk so
// listing doesn't open a ProjectContext per chat.
func (r *ProjectRegistry) ListChats() []protocol.SessionSummary {
	entries, _ := os.ReadDir(r.chatsDir)
	out := []protocol.SessionSummary{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		store, err := agent.NewDiskSessionStore(projectstore.DataDir(r.stateDir, filepath.Join(r.chatsDir, e.Name())))
		if err != nil {
			logWarn("list chats: skipping %s: %v", e.Name(), err)
			continue
		}
		ids, err := store.List()
		if err != nil {
			logWarn("list chats: cannot list %s: %v", e.Name(), err)
		}
		for _, id := range ids {
			st, err := store.Load(id)
			if err != nil {
				// Left out of the list, so the chat vanishes from every client: say why.
				logWarn("list chats: skipping unreadable session %s in %s: %v", id, e.Name(), err)
				continue
			}
			sum := sessionSummaryFrom(filepath.Join(r.chatsDir, e.Name()), st)
			sum.WorkspaceBytes = dirSize(sum.ProjectPath)
			out = append(out, sum)
		}
	}
	return out
}

// DropChat forgets a deleted chat's context and removes its scratch root and
// state. Callers have already cancelled its session.
func (r *ProjectRegistry) DropChat(root string) {
	if !r.isChat(root) {
		return
	}
	r.mu.Lock()
	delete(r.projects, root)
	r.mu.Unlock()
	_ = os.RemoveAll(root)
	_ = os.RemoveAll(projectstore.DataDir(r.stateDir, root))
}

// dirSize is the total size of root's regular files, counting at most
// dirSizeMaxEntries entries so a chat that unpacked a huge tree can't make
// listing slow; past that it reports what it had counted.
// ponytail: a lower bound for very large workspaces, a cached size if it matters.
const dirSizeMaxEntries = 20000

func dirSize(root string) int64 {
	var total int64
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > dirSizeMaxEntries {
			return fs.SkipAll
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// pruneChats removes scratch roots left by a chat that was set up but never
// prompted (set_mode/set_provider create the directory before the first
// message): no session on disk and untouched for a day. Runs at startup only,
// so it can't race a chat being created in this process.
func pruneChats(stateDir, chatsDir string) {
	entries, _ := os.ReadDir(chatsDir)
	for _, e := range entries {
		root := filepath.Join(chatsDir, e.Name())
		info, err := e.Info()
		if !e.IsDir() || err != nil || time.Since(info.ModTime()) < 24*time.Hour {
			continue
		}
		data := projectstore.DataDir(stateDir, root)
		if store, err := agent.NewDiskSessionStore(data); err == nil {
			if ids, _ := store.List(); len(ids) > 0 {
				continue
			}
		}
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(data)
	}
}
