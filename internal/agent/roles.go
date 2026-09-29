package agent

import (
	"context"
	"sync"
)

// Model roles (SPEC.md §4f) say which model runs which kind of work. The
// session's own provider is the main one; every other role falls back to it
// when unassigned, so a server with no roles behaves exactly as before.
type Role string

const (
	RolePlan    Role = "plan"    // turns in plan mode
	RoleExplore Role = "explore" // subagents with kind "explore"
	RoleUtility Role = "utility" // titles and compaction summaries
	RoleAdvisor Role = "advisor" // Anthropic's advisor tool, on main-model turns
)

// RoleBinding is the provider a turn, subagent or utility call runs on.
type RoleBinding struct {
	Provider     ModelProvider
	ProviderName string
	Model        string
}

// Roles is the server-wide role table, shared by every session and safe
// for concurrent use. A nil *Roles has no roles assigned.
type Roles struct {
	mu sync.RWMutex
	m  map[Role]RoleBinding
}

func NewRoles() *Roles { return &Roles{m: map[Role]RoleBinding{}} }

// Set replaces the whole table.
func (r *Roles) Set(m map[Role]RoleBinding) {
	cp := make(map[Role]RoleBinding, len(m))
	for k, v := range m {
		cp[k] = v
	}
	r.mu.Lock()
	r.m = cp
	r.mu.Unlock()
}

func (r *Roles) Get(role Role) (RoleBinding, bool) {
	if r == nil {
		return RoleBinding{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.m[role]
	return b, ok && b.Provider != nil
}

// RequestHints are per-request facts a provider may act on, carried in the
// context so ModelProvider's signature stays the same for every provider.
type RequestHints struct {
	// CacheKey is stable for a session, so a provider that routes by it
	// (OpenAI's prompt_cache_key) lands consecutive requests on a warm cache.
	CacheKey string
	// AdvisorModel is the model for Anthropic's advisor tool; "" for none.
	AdvisorModel string
}

type requestHintsKey struct{}

func WithRequestHints(ctx context.Context, h RequestHints) context.Context {
	return context.WithValue(ctx, requestHintsKey{}, h)
}

func RequestHintsFrom(ctx context.Context) RequestHints {
	h, _ := ctx.Value(requestHintsKey{}).(RequestHints)
	return h
}

// modelNameOf reports p's model id, or "" if it doesn't expose one.
func modelNameOf(p ModelProvider) string {
	if n, ok := p.(interface{ ModelName() string }); ok {
		return n.ModelName()
	}
	return ""
}

// turnBinding picks the provider a turn runs on: the plan role in plan
// mode, when one is assigned, the session's own provider otherwise. A
// native CLI agent routes its own models, so roles never replace it.
func (a *Agent) turnBinding() RoleBinding {
	a.mu.Lock()
	main := RoleBinding{Provider: a.provider, ProviderName: a.providerName}
	mode, roles := a.mode, a.roles
	a.mu.Unlock()
	main.Model = modelNameOf(main.Provider)
	if _, native := main.Provider.(NativeAgent); native {
		return main
	}
	if mode == ModePlan {
		if b, ok := roles.Get(RolePlan); ok {
			return b
		}
	}
	return main
}

// roleOr is role's binding, or fallback when it isn't assigned.
func (a *Agent) roleOr(role Role, fallback RoleBinding) RoleBinding {
	a.mu.Lock()
	roles := a.roles
	a.mu.Unlock()
	if b, ok := roles.Get(role); ok {
		return b
	}
	return fallback
}
