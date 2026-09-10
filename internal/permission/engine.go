package permission

import (
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
	mu           sync.RWMutex
	sessionCache map[string]bool // cacheKey -> allowed (true/false)
	normalizer   *normalizer.Normalizer
}

func New(norm *normalizer.Normalizer) *Engine {
	return &Engine{
		sessionCache: make(map[string]bool),
		normalizer:   norm,
	}
}

// Evaluate determines whether a tool execution is allowed, denied, or requires human authorization.
func (e *Engine) Evaluate(toolName string, rawCommand string) (Decision, string) {
	cacheKey := toolName + ":" + rawCommand

	e.mu.RLock()
	cached, found := e.sessionCache[cacheKey]
	e.mu.RUnlock()

	if found {
		if cached {
			return DecisionAllow, cacheKey
		}
		return DecisionDeny, cacheKey
	}

	// Rule 1: Read-only operations are unconditionally allowed
	if toolName == "file_read" {
		return DecisionAllow, cacheKey
	}

	// Rule 2: Bash commands
	if toolName == "bash" {
		parsed, err := e.normalizer.ParseShellArgv(rawCommand)
		if err != nil {
			return DecisionDeny, cacheKey
		}

		// Destructive bash is forced to ASK
		if e.normalizer.IsDestructiveBash(parsed) {
			return DecisionAsk, cacheKey
		}

		// Basic safe commands (ls, pwd, git status) can be allowed by policy
		if len(parsed.Argv) > 0 {
			switch parsed.Argv[0] {
			case "ls", "pwd", "echo", "whoami":
				return DecisionAllow, cacheKey
			case "git":
				if len(parsed.Argv) > 1 && (parsed.Argv[1] == "status" || parsed.Argv[1] == "diff") {
					return DecisionAllow, cacheKey
				}
			}
		}

		// Fallthrough for bash defaults to ASK
		return DecisionAsk, cacheKey
	}

	// Default fallback rule for unknown tools
	return DecisionAsk, cacheKey
}

func (e *Engine) RecordDecision(cacheKey string, allowed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessionCache[cacheKey] = allowed
}

func (e *Engine) ClearCache() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessionCache = make(map[string]bool)
}
