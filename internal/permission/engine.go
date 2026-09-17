package permission

import (
	"strings"
	"sync"
	"yoakeland/internal/normalizer"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

type Engine struct {
	mu             sync.RWMutex
	sessionCache   map[string]bool
	persistedCache map[string]bool
	store          *PersistentStore
	normalizer     *normalizer.Normalizer
}

func New(norm *normalizer.Normalizer, store *PersistentStore) *Engine {
	e := &Engine{
		sessionCache:   make(map[string]bool),
		persistedCache: make(map[string]bool),
		store:          store,
		normalizer:     norm,
	}

	if store != nil {
		if loaded, err := store.Load(); err == nil {
			e.persistedCache = loaded
		}
	}
	return e
}

// Evaluate decides whether a tool call is allowed, denied, or needs to ask,
// and returns the cache key a "remember" decision (§3.1) is stored under.
// The key is an exact toolName+target match, not a glob or path prefix —
// "remember" means "this exact command / this exact file path again", not
// "anything under this folder" or "this command with any arguments". A
// broader pattern would need real matching logic (and a decision about what
// scope is actually safe to remember) that doesn't exist yet.
func (e *Engine) Evaluate(toolName string, target string) (Decision, string) {
	cacheKey := toolName + ":" + target

	e.mu.RLock()
	// 1. Session decision overrides
	if allowed, exists := e.sessionCache[cacheKey]; exists {
		e.mu.RUnlock()
		if allowed {
			return DecisionAllow, cacheKey
		}
		return DecisionDeny, cacheKey
	}

	// 2. Project-persisted decisions (.yoake/permissions.json)
	if allowed, exists := e.persistedCache[cacheKey]; exists {
		e.mu.RUnlock()
		if allowed {
			return DecisionAllow, cacheKey
		}
		return DecisionDeny, cacheKey
	}
	e.mu.RUnlock()

	// 3. Static policy rules
	if toolName == "file_read" {
		return DecisionAllow, cacheKey
	}

	if toolName == "bash" {
		parsed, err := e.normalizer.ParseShellArgv(target)
		if err != nil {
			return DecisionDeny, cacheKey
		}
		if e.normalizer.IsDestructiveBash(parsed) {
			return DecisionAsk, cacheKey
		}
		if len(parsed.Argv) > 0 {
			switch parsed.Argv[0] {
			case "ls":
				// Unlike pwd/whoami/echo, ls's arguments are attacker-
				// meaningful paths it can read/list — only auto-allow when
				// every non-flag argument stays inside the project (or
				// there are none, i.e. bare "ls"). "ls /etc", "ls ../../etc"
				// etc. fall through to Ask like any other command.
				if lsArgsStayInProject(e.normalizer, parsed.Argv[1:]) {
					return DecisionAllow, cacheKey
				}
			case "pwd", "echo", "whoami":
				return DecisionAllow, cacheKey
			case "git":
				if len(parsed.Argv) > 1 && (parsed.Argv[1] == "status" || parsed.Argv[1] == "diff") {
					return DecisionAllow, cacheKey
				}
			}
		}
		return DecisionAsk, cacheKey
	}

	// Mutating tools (file_write, file_edit) default to ask unless recorded
	if toolName == "file_write" || toolName == "file_edit" {
		return DecisionAsk, cacheKey
	}

	return DecisionAsk, cacheKey
}

// lsArgsStayInProject reports whether every non-flag argument to `ls`
// resolves inside the project root — the same containment check
// file_read/file_write/file_edit already use — so bare `ls`, `ls .`,
// `ls src/` keep auto-approving while `ls /etc`, `ls ../../etc`, `ls ~`
// don't. Flags (anything starting with "-") are skipped rather than
// resolved as paths.
func lsArgsStayInProject(norm *normalizer.Normalizer, args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if _, err := norm.ResolveAndValidatePath(arg); err != nil {
			return false
		}
	}
	return true
}

func (e *Engine) RecordDecision(cacheKey string, allowed bool, persist bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.sessionCache[cacheKey] = allowed
	if persist && e.store != nil {
		e.persistedCache[cacheKey] = allowed
		_ = e.store.Save(e.persistedCache)
	}
}

func (e *Engine) ClearSessionCache() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessionCache = make(map[string]bool)
}
