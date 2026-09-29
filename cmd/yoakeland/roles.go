package main

import (
	"fmt"
	"sort"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// Model roles (SPEC.md §4f) are server-wide: set once with set_model_roles,
// persisted in server.json, and shared by every session of every project.

var knownRoles = map[string]agent.Role{
	"plan":    agent.RolePlan,
	"explore": agent.RoleExplore,
	"utility": agent.RoleUtility,
	"advisor": agent.RoleAdvisor,
}

// buildRoles turns a role table into live providers. Roles run the plain
// API providers only: a CLI agent brings its own loop and can't serve as a
// subagent, summarizer or advisor. The advisor is Anthropic's server-side
// tool, so it has to name an Anthropic model.
func buildRoles(store *provider.ProviderStore, table map[string]protocol.ModelRole) (map[agent.Role]agent.RoleBinding, error) {
	out := make(map[agent.Role]agent.RoleBinding, len(table))
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names) // a stable first error
	for _, name := range names {
		r := table[name]
		role, ok := knownRoles[name]
		if !ok {
			return nil, fmt.Errorf("unknown model role %q (known: plan, explore, utility, advisor)", name)
		}
		cfg, ok := store.Get(r.ProviderName)
		if !ok {
			return nil, fmt.Errorf("%s role: unknown provider %q", name, r.ProviderName)
		}
		if cfg.Kind != provider.ProviderKindAPIKey {
			return nil, fmt.Errorf("%s role: %q is a CLI agent; roles take an API provider", name, r.ProviderName)
		}
		if role == agent.RoleAdvisor && (cfg.HTTPProvider != "anthropic" || r.Model == "") {
			return nil, fmt.Errorf("advisor role: needs an Anthropic provider and a model")
		}
		built, err := provider.BuildProvider(cfg, r.Model, "")
		if err != nil {
			return nil, fmt.Errorf("%s role: %v", name, err)
		}
		if setter, ok := built.(agent.ModelOptionsSetter); ok && r.ReasoningEffort != "" {
			setter.SetModelOptions(agent.ModelOptions{ReasoningEffort: r.ReasoningEffort})
		}
		model := r.Model
		if m, ok := built.(interface{ ModelName() string }); ok {
			model = m.ModelName()
		}
		out[role] = agent.RoleBinding{Provider: built, ProviderName: r.ProviderName, Model: model}
	}
	return out, nil
}

// reloadRoles rebuilds the live role table from the persisted one — at
// startup, and after a provider's credentials change or it's removed. A
// role that no longer builds is dropped (it falls back to each session's
// own model) rather than keeping a stale provider.
func reloadRoles(roles *agent.Roles, store *provider.ProviderStore, settings *ServerSettings) {
	table := settings.GetModelRoles()
	live := map[agent.Role]agent.RoleBinding{}
	for name, r := range table {
		b, err := buildRoles(store, map[string]protocol.ModelRole{name: r})
		if err != nil {
			logWarn("model roles: %v — that role falls back to each session's own model", err)
			continue
		}
		for k, v := range b {
			live[k] = v
		}
	}
	roles.Set(live)
}
