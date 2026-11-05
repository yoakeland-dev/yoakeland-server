package main

import (
	"context"

	"yoakeland/internal/agent"
	"yoakeland/internal/provider"
)

// cliProviderNames are the registered providers that are CLI agents, which run under the owner's
// login rather than the sandbox: the names the cli cap lists.
func (d connDeps) cliProviderNames() []string {
	var out []string
	if d.providerStore == nil {
		return out
	}
	for _, p := range d.providerStore.List() {
		if _, cli := provider.CLIKindOf(p.Kind); cli {
			out = append(out, p.Name)
		}
	}
	return out
}

// syncPeerAccess re-reads every TCP connection's tier from its credential's person, so a change
// applies at once, and tells each device whose tier or person changed.
func (d connDeps) syncPeerAccess() []*Peer {
	changed := d.peers.SyncAccess(func(cred string) (string, string) {
		a := d.settings.Access("tcp", cred)
		return a.Tier, a.UserID
	})
	for _, p := range changed {
		sendServerIdentity(d, p)
	}
	return changed
}

// turnPolicyFor is the policy for a turn started by a sender with acc: none for the owner and Full
// people, else what their capabilities allow.
func turnPolicyFor(acc Access) *agent.TurnPolicy {
	if acc.Full() {
		return nil
	}
	return &agent.TurnPolicy{
		Commands: acc.Caps.CommandsSandboxed, Unsandboxed: acc.Caps.CommandsUnsandboxed, Autonomous: acc.Caps.Autonomous,
		FilesEdit: acc.Caps.FilesEdit, MCP: acc.Caps.MCPTools, Browser: acc.Caps.Browser == browserUse,
		AllowCLI: acc.Caps.AllowCLI, UserID: acc.UserID,
	}
}

// actorOf is who a connection's turns are attributed to: the owner on the local socket, else the
// credential's person.
func (d connDeps) actorOf(network, peerID string) agent.TurnActor {
	if network != "tcp" {
		return agent.TurnActor{UserID: ownerID}
	}
	acc := d.accessFor(network, peerID)
	cred := ""
	if p, ok := d.peers.Get(peerID); ok {
		cred = p.TokenName
	}
	return agent.TurnActor{UserID: acc.UserID, Credential: cred}
}

// turnContext marks ctx with the sender of a turn and the policy their access gives it. Every path
// that starts a turn on a sender's behalf goes through here.
func (d connDeps) turnContext(ctx context.Context, network, peerID string) context.Context {
	return d.turnContextFor(ctx, d.actorOf(network, peerID), d.accessFor(network, peerID))
}

func (d connDeps) turnContextFor(ctx context.Context, who agent.TurnActor, acc Access) context.Context {
	return agent.WithTurnPolicy(agent.WithTurnActor(ctx, who), turnPolicyFor(acc))
}

// accessOfActor is the access a turn's actor has now. False when the credential or its person is
// gone: whatever they started must not run.
func (d connDeps) accessOfActor(who agent.TurnActor) (Access, bool) {
	if who.Credential == "" {
		if who.UserID == "" || who.UserID == ownerID {
			return localAccess(), true
		}
		return Access{}, false
	}
	if !d.settings.credentialLive(who.Credential) {
		return Access{}, false
	}
	return d.settings.Access("tcp", who.Credential), true
}

// accessSnapshot is every credential's access now, to compare around a change.
func (d connDeps) accessSnapshot() map[string]Access { return d.settings.AccessSnapshot() }

// enforceAccessChange cancels the running turns of every credential whose access was lowered or
// removed between before and after, and re-syncs the live connections. It returns how many turns
// it cancelled. Call it after any change to people, credentials or restrictions, with the snapshot
// taken before the change.
func (d connDeps) enforceAccessChange(before map[string]Access) int {
	after := d.accessSnapshot()
	lowered := map[string]bool{}
	for cred, was := range before {
		now, still := after[cred]
		if !still || accessLowered(was, now) {
			lowered[cred] = true
		}
	}
	d.syncPeerAccess()
	if len(lowered) == 0 {
		return 0
	}
	return d.projects.CancelTurnsWhere(func(a agent.TurnActor) bool { return lowered[a.Credential] })
}

// accessLowered reports whether now can do less than was in any way: a lower tier, a cap turned
// off, a CLI agent dropped, or the browser lowered.
func accessLowered(was, now Access) bool {
	if tierRank(now.Tier) < tierRank(was.Tier) {
		return true
	}
	w, n := was.Caps, now.Caps
	for _, c := range []string{CapFilesEdit, CapCommandsSandboxed, CapCommandsUnsandboxed, CapAutonomous, CapMCPTools, CapTerminalsWatch, CapProcesses, CapOthersChats} {
		if w.Has(c) && !n.Has(c) {
			return true
		}
	}
	browserRank := map[string]int{browserOff: 0, browserWatch: 1, browserUse: 2}
	if browserRank[n.Browser] < browserRank[w.Browser] {
		return true
	}
	if w.AllCLI && !n.AllCLI {
		return true
	}
	for _, name := range w.CLI {
		if !n.AllowCLI(name) {
			return true
		}
	}
	return false
}
