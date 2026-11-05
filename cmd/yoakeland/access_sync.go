package main

import "yoakeland/internal/provider"

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
