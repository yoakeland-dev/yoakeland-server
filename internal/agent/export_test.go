package agent

// WaitBackground blocks until this Agent's title-generation goroutines have
// finished writing, so a test's TempDir cleanup doesn't race them.
func (a *Agent) WaitBackground() { a.titleWG.Wait() }

// WaitBackground is Agent.WaitBackground for every Agent the registry holds.
func (r *Registry) WaitBackground() {
	r.mu.Lock()
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.WaitBackground()
	}
}

// SetHooks installs hooks directly (the server does it via Registry.SetHooks).
func (a *Agent) SetHooks(h Hooks) { a.hooks = h }

// PermissionTarget exposes permissionTarget to the external test package.
var PermissionTarget = permissionTarget
