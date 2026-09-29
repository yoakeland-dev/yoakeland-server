package agent

import "context"

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

// AskNative and AskQuestions expose the native-agent asks to the external test package.
func (a *Agent) AskNative(ctx context.Context, sessionID, turnID string, req NativeAsk) (NativeAnswer, error) {
	return a.askNativePermission(ctx, sessionID, turnID, req)
}

func (a *Agent) AskQuestions(ctx context.Context, sessionID, turnID string, qs []NativeQuestion) ([]string, error) {
	return a.askUserQuestions(ctx, sessionID, turnID, qs)
}
