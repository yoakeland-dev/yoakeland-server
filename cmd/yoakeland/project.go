package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// ProjectContext bundles everything scoped to one project root: its own
// sandbox boundary (normalizer + sandbox), permission engine, snapshot and
// session stores, and its own session registry. Two projects never share
// any of this — a tool call in one project's sandbox can't touch another
// project's files, and one project's sessions are invisible to another's.
type ProjectContext struct {
	Root        string
	SnapStore   snapshot.SnapshotStore
	SessStore   agent.SessionStore
	Registry    *agent.Registry
	SandboxTier string
}

func newProjectContext(root string, defaultProvider agent.ModelProvider, defaultProviderName string, maxSessionSpendUSD float64) (*ProjectContext, error) {
	norm, err := normalizer.New(root)
	if err != nil {
		return nil, fmt.Errorf("normalizer: %w", err)
	}

	permStore := permission.NewPersistentStore(norm.ProjectRoot)
	permEngine := permission.New(norm, permStore)

	snapStore, err := snapshot.NewDiskSnapshotStore(norm.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}

	sessStore, err := agent.NewDiskSessionStore(norm.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}

	sb := sandbox.NewSandbox(norm.ProjectRoot, 60*time.Second)
	if sb.Mode() == sandbox.ModeLocalRestr {
		logWarnDetail(
			fmt.Sprintf("no sandbox isolation for project %q — bash tool runs unconfined", norm.ProjectRoot),
			fmt.Sprintf("no sandbox isolation available for project %q (bubblewrap and Linux namespaces both unavailable) — the bash tool runs with NO process/filesystem isolation beyond the project-root path check; a command can see and touch anything this OS user can. Install bubblewrap (bwrap) for real isolation.", norm.ProjectRoot),
		)
	}
	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
	}
	braveKey := os.Getenv("BRAVE_API_KEY")
	if braveKey == "" {
		logWarnDetail(
			"web_search tool using DuckDuckGo fallback: no BRAVE_API_KEY",
			"BRAVE_API_KEY is not set, so the web_search tool falls back to scraping DuckDuckGo, which has no setup cost but can intermittently fail bot checks. Get a key at https://brave.com/search/api/ and set BRAVE_API_KEY for reliable search.",
		)
	}
	tools = append(tools, tool.NewWebSearchTool(braveKey), tool.NewWebFetchTool())

	ctxMgr := agent.NewContextManager(64000, 4000)
	usageStore, err := agent.NewUsageStore(norm.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("usage store: %w", err)
	}

	registry := agent.NewRegistry(defaultProvider, defaultProviderName, tools, permEngine, snapStore, sessStore, ctxMgr, usageStore)
	registry.SetMaxSessionSpendUSD(maxSessionSpendUSD)

	return &ProjectContext{
		Root:        norm.ProjectRoot,
		SnapStore:   snapStore,
		SessStore:   sessStore,
		Registry:    registry,
		SandboxTier: string(sb.Mode()),
	}, nil
}

// toHistoryMessages converts a session's stored agent.Message history into
// the wire shape for get_session_history's response. agent.Message can't be
// used directly on the wire since internal/protocol can't import
// internal/agent (agent already imports protocol).
func toHistoryMessages(history []agent.Message) []protocol.HistoryMessage {
	out := make([]protocol.HistoryMessage, 0, len(history))
	for _, m := range history {
		hm := protocol.HistoryMessage{
			Role:         m.Role,
			Content:      m.Content,
			Reasoning:    m.Reasoning,
			ToolCallID:   m.ToolCallID,
			Author:       m.AuthorName,
			AuthorAvatar: m.AuthorAvatar,
			Provider:     m.Provider,
			Model:        m.Model,
			CreatedAt:    m.CreatedAt,
			GenerationMs: m.GenerationMs,
			Usage:        m.Usage,
		}
		for _, tc := range m.ToolCalls {
			hm.ToolCalls = append(hm.ToolCalls, protocol.HistoryToolCall{
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Args,
			})
		}
		out = append(out, hm)
	}
	return out
}

// sessionSummaryFrom builds the wire summary shared by session_list and
// session_updated from a persisted session's state.
func sessionSummaryFrom(state *agent.SessionState) protocol.SessionSummary {
	return protocol.SessionSummary{
		SessionID:       state.SessionID,
		UpdatedAt:       state.UpdatedAt,
		MessageCount:    len(state.History),
		Title:           state.Title,
		Archived:        state.Archived,
		LinkedSessionID: state.LinkedSessionID,
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
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolving home directory: %w", err)
		}
		path = home
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
	maxSpendUSD         float64
	known               *projectstore.Store // persisted "which projects exist", server-wide
}

// NewProjectRegistry builds a project registry backed by a persisted list of
// known projects under stateDir (<-state-dir>/projects.json, alongside
// providers.json) — this is what makes list_projects reflect every project
// this server has ever opened, not just ones opened in the current process's
// lifetime.
func NewProjectRegistry(defaultProvider agent.ModelProvider, defaultProviderName string, maxSessionSpendUSD float64, stateDir string) (*ProjectRegistry, error) {
	known, err := projectstore.New(stateDir)
	if err != nil {
		return nil, fmt.Errorf("project list store: %w", err)
	}
	return &ProjectRegistry{
		projects:            make(map[string]*ProjectContext),
		sessionProject:      make(map[string]string),
		defaultProvider:     defaultProvider,
		defaultProviderName: defaultProviderName,
		maxSpendUSD:         maxSessionSpendUSD,
		known:               known,
	}, nil
}

// projectKey canonicalizes root the same way normalizer.New does (symlinks
// resolved, not just made absolute) so the registry's map key always agrees
// with ProjectContext.Root — otherwise a symlinked path and its real-path
// equivalent silently become two independent ProjectContexts pointed at the
// same on-disk .yoake directory.
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

	pc, err := newProjectContext(key, r.defaultProvider, r.defaultProviderName, r.maxSpendUSD)
	if err != nil {
		return nil, err
	}
	r.projects[key] = pc

	// Only recorded on an actual fresh open (not the map-hit fast path
	// above), so a hot session doesn't turn every envelope it touches into
	// a projects.json rewrite. ponytail: LastOpenedAt only advances once
	// per process-lifetime per project, not per request — fine for a
	// "recent projects" sort, not meant as a precise last-touched clock.
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
// alive. It does not touch anything on disk under the project root itself
// (.yoake/ stays put); "remove" means "stop tracking," not "delete."
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
		}
	}

	return r.known.Remove(key)
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

// Resolve figures out which project a session-scoped envelope refers to:
//   - an explicit path wins, and binds sessionID to it going forward
//   - a path conflicting with the session's existing binding is an error —
//     a session's project is fixed for its lifetime, not reassignable
//   - otherwise fall back to the session's existing binding
//   - otherwise fall back to defaultRoot (the server's -project, if any)
//   - otherwise it's an error: nothing to resolve against
func (r *ProjectRegistry) Resolve(sessionID, explicitPath, defaultRoot string) (*ProjectContext, error) {
	if explicitPath != "" {
		key, err := projectKey(explicitPath)
		if err != nil {
			return nil, fmt.Errorf("resolving project path %q: %w", explicitPath, err)
		}

		r.mu.Lock()
		if bound, ok := r.sessionProject[sessionID]; ok && bound != key {
			r.mu.Unlock()
			return nil, fmt.Errorf("session %s is already bound to project %q, not %q", sessionID, bound, key)
		}
		r.sessionProject[sessionID] = key
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
