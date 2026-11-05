package agent

import (
	"context"
	"fmt"
	"strings"

	"yoakeland/internal/tool"
)

// browserServerPrefix names the tools of yoakeland's own MCP server for CLI agents.
const browserServerPrefix = "mcp__yoake-browser__"

// TurnPolicy holds the rules that depend on who started a turn (PLAN_PERMISSIONS "Turn policy").
// A nil policy means unrestricted: the server sets one only for a sender below the Full tier, and
// the local socket never gets one. It rides in the turn's context, so the turn, its subagents and
// anything the turn starts inherit it. Tool definitions and tool execution both check it, and an
// approval at a permission prompt can't raise it.
type TurnPolicy struct {
	Commands    bool // bash and terminal_* on a project with a real sandbox
	Unsandboxed bool // the same on a project with no sandbox (local_restricted_fallback)
	Autonomous  bool // modes autonomous and auto, when commands are sandboxed
	FilesEdit   bool // false: the turn runs read-only
	MCP         bool // mcp__* tools
	Browser     bool // the browser tool
	// AllowCLI reports whether the CLI agent registered as providerName may run this turn's tools;
	// nil allows none.
	AllowCLI func(providerName string) bool
	UserID   string
}

type turnPolicyKey struct{}

// WithTurnPolicy attaches p to ctx for the turns it starts. A nil p is no restriction.
func WithTurnPolicy(ctx context.Context, p *TurnPolicy) context.Context {
	return context.WithValue(ctx, turnPolicyKey{}, p)
}

// TurnPolicyFrom is the policy ctx carries, nil if none.
func TurnPolicyFrom(ctx context.Context) *TurnPolicy {
	p, _ := ctx.Value(turnPolicyKey{}).(*TurnPolicy)
	return p
}

// TurnActor says whose credential started a turn, so the server can cancel a person's or a
// device's running turns when its access is revoked or lowered. It is set for every sender, the
// owner included, and is independent of the policy (which the owner doesn't have).
type TurnActor struct {
	UserID     string
	Credential string
}

type turnActorKey struct{}

func WithTurnActor(ctx context.Context, a TurnActor) context.Context {
	return context.WithValue(ctx, turnActorKey{}, a)
}

func turnActorFrom(ctx context.Context) TurnActor {
	a, _ := ctx.Value(turnActorKey{}).(TurnActor)
	return a
}

// effectiveMode is the mode a turn runs in: the stored one unless the policy forbids it. Autonomous
// and auto need the autonomous cap and commands in a real sandbox, else the turn runs as
// read_write; without files_edit nothing but read_only and plan is allowed, so it runs as
// read_only. The stored mode is never changed.
func (p *TurnPolicy) effectiveMode(stored Mode, unsandboxed bool) Mode {
	if p == nil {
		return stored
	}
	m := stored
	if (m == ModeAutonomous || m == ModeAuto) && (!p.Autonomous || unsandboxed) {
		m = ModeReadWrite
	}
	if !p.FilesEdit && m != ModeReadOnly && m != ModePlan {
		m = ModeReadOnly
	}
	return m
}

// blocks reports whether the policy withholds the tool name: shell tools without the command cap
// that fits the project, mcp__* tools without MCP, the browser tool without Browser.
func (p *TurnPolicy) blocks(name string, unsandboxed bool) bool {
	if p == nil {
		return false
	}
	switch {
	case isShellTool(strings.ToLower(name)): // a CLI agent names its own shell tool ("Bash")
		if unsandboxed {
			return !p.Unsandboxed
		}
		return !p.Commands
	case name == "browser":
		return !p.Browser
	case name == browserServerPrefix+tool.TodoToolName || name == browserServerPrefix+tool.ArtifactLogsToolName:
		return false // yoakeland's own server also carries the todo list and a page's log, which change nothing
	case strings.HasPrefix(name, browserServerPrefix):
		return !p.Browser // the browser tool as a CLI agent sees it
	case isMCPTool(name):
		return !p.MCP
	}
	return false
}

func (p *TurnPolicy) allowsCLI(providerName string) bool {
	return p == nil || (p.AllowCLI != nil && p.AllowCLI(providerName))
}

// cliRefusal is the error for a turn bound to a CLI agent the policy doesn't allow.
func cliRefusal(providerName string) error {
	return fmt.Errorf("the CLI agent %q isn't allowed for this person: it runs under the owner's login with its own tools", providerName)
}

// SetUnsandboxed marks the sessions of a project whose commands run with no sandbox
// (local_restricted_fallback), which turn policies treat differently.
func (r *Registry) SetUnsandboxed(unsandboxed bool) {
	r.mu.Lock()
	r.unsandboxed = unsandboxed
	agents := make([]*Agent, 0, len(r.sessions))
	for _, ag := range r.sessions {
		agents = append(agents, ag)
	}
	r.mu.Unlock()
	for _, ag := range agents {
		ag.mu.Lock()
		ag.unsandboxed = unsandboxed
		ag.mu.Unlock()
	}
}

// CancelTurnsWhere cancels every running or queued turn, in any session, whose actor match
// accepts, and returns how many. Used when someone's access is revoked or lowered.
func (r *Registry) CancelTurnsWhere(match func(TurnActor) bool) int {
	n := 0
	for _, ag := range r.agents() {
		n += ag.cancelTurnsWhere(match)
	}
	return n
}

func (a *Agent) cancelTurnsWhere(match func(TurnActor) bool) int {
	a.mu.Lock()
	var cancels []context.CancelFunc
	for id, actor := range a.turnActors {
		if !match(actor) {
			continue
		}
		if c, ok := a.activeTurnCancels[id]; ok {
			cancels = append(cancels, c)
		} else if c, ok := a.queuedTurns[id]; ok {
			cancels = append(cancels, c)
		}
	}
	a.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// effectiveModeLocked is the mode the current turn runs in. Callers hold a.mu.
func (a *Agent) effectiveModeLocked() Mode { return a.policy.effectiveMode(a.mode, a.unsandboxed) }

// turnMode is the mode the current turn runs in; Mode() stays the stored one.
func (a *Agent) turnMode() Mode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.effectiveModeLocked()
}
