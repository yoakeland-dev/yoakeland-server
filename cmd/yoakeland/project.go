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

func newProjectContext(root string, defaultProvider agent.ModelProvider, maxSessionSpendUSD float64) (*ProjectContext, error) {
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
	if braveKey := os.Getenv("BRAVE_API_KEY"); braveKey != "" {
		tools = append(tools, tool.NewWebSearchTool(braveKey))
	} else {
		logWarnDetail(
			"web_search tool disabled: no BRAVE_API_KEY",
			"BRAVE_API_KEY is not set, so the web_search tool was not registered for this project. Get a key at https://brave.com/search/api/ and set BRAVE_API_KEY to enable it.",
		)
	}

	ctxMgr := agent.NewContextManager(64000, 4000)
	usageStore, err := agent.NewUsageStore(norm.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("usage store: %w", err)
	}

	registry := agent.NewRegistry(defaultProvider, tools, permEngine, snapStore, sessStore, ctxMgr, usageStore)
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
	mu              sync.Mutex
	projects        map[string]*ProjectContext // key: cleaned absolute path
	sessionProject  map[string]string          // sessionID -> project key
	defaultProvider agent.ModelProvider
	maxSpendUSD     float64
}

func NewProjectRegistry(defaultProvider agent.ModelProvider, maxSessionSpendUSD float64) *ProjectRegistry {
	return &ProjectRegistry{
		projects:        make(map[string]*ProjectContext),
		sessionProject:  make(map[string]string),
		defaultProvider: defaultProvider,
		maxSpendUSD:     maxSessionSpendUSD,
	}
}

func projectKey(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
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

	pc, err := newProjectContext(key, r.defaultProvider, r.maxSpendUSD)
	if err != nil {
		return nil, err
	}
	r.projects[key] = pc
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

// List returns every project opened on this server process since it
// started (not a filesystem scan, not persisted).
func (r *ProjectRegistry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.projects))
	for key := range r.projects {
		out = append(out, key)
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
