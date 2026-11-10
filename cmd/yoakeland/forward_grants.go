package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

// Lending (PLAN_FORWARDING "Lending is a grant"). A request to run a provider here comes from some
// other server, through the person's device; without a grant a server the person is only a guest
// on could spend their keys by asking. So the person says, on this server, which providers each
// other server may use as them. A grant is theirs: it is kept under their user id.

const (
	maxGrantsPerPerson = 256
	maxServerNameRunes = 80
)

// forwardLoan is what one person lends to one other server.
type forwardLoan struct {
	Name      string   `json:"name,omitempty"` // the server's name when it was granted, a label only
	Providers []string `json:"providers"`
}

var errTooManyGrants = errors.New("too many forwarding grants")

// GrantForward lets serverID use provider as userID. Idempotent; the label is refreshed.
func (s *ServerSettings) GrantForward(userID, serverID, serverName, provider string) error {
	if r := []rune(strings.TrimSpace(cleanName(serverName))); len(r) > maxServerNameRunes {
		serverName = string(r[:maxServerNameRunes])
	} else {
		serverName = string(r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	loan := s.ForwardGrants[userID][serverID]
	if !slices.Contains(loan.Providers, provider) {
		n := 0
		for _, l := range s.ForwardGrants[userID] {
			n += len(l.Providers)
		}
		if n >= maxGrantsPerPerson {
			return errTooManyGrants
		}
		loan.Providers = append(slices.Clone(loan.Providers), provider)
	}
	if serverName != "" {
		loan.Name = serverName
	}
	if s.ForwardGrants == nil {
		s.ForwardGrants = map[string]map[string]forwardLoan{}
	}
	if s.ForwardGrants[userID] == nil {
		s.ForwardGrants[userID] = map[string]forwardLoan{}
	}
	s.ForwardGrants[userID][serverID] = loan
	return s.save()
}

// RevokeForward takes a grant back. Idempotent.
func (s *ServerSettings) RevokeForward(userID, serverID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	loan, ok := s.ForwardGrants[userID][serverID]
	if !ok || !slices.Contains(loan.Providers, provider) {
		return nil
	}
	loan.Providers = slices.DeleteFunc(slices.Clone(loan.Providers), func(p string) bool { return p == provider })
	if len(loan.Providers) == 0 {
		delete(s.ForwardGrants[userID], serverID)
		if len(s.ForwardGrants[userID]) == 0 {
			delete(s.ForwardGrants, userID)
		}
	} else {
		s.ForwardGrants[userID][serverID] = loan
	}
	return s.save()
}

// ForwardGranted reports whether userID lent provider to serverID.
func (s *ServerSettings) ForwardGranted(userID, serverID, provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.ForwardGrants[userID][serverID].Providers, provider)
}

// ListForwardGrants lists userID's grants, sorted by server name then provider name.
func (s *ServerSettings) ListForwardGrants(userID string) []protocol.ForwardGrantData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []protocol.ForwardGrantData{}
	for sid, loan := range s.ForwardGrants[userID] {
		for _, p := range loan.Providers {
			out = append(out, protocol.ForwardGrantData{ServerID: sid, ServerName: loan.Name, ProviderName: p})
		}
	}
	slices.SortFunc(out, func(a, b protocol.ForwardGrantData) int {
		return cmpStrings(a.ServerName, b.ServerName, a.ProviderName, b.ProviderName, a.ServerID, b.ServerID)
	})
	return out
}

func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

// dropForwardProvider forgets every grant of the provider name, for everyone: it is gone.
func (s *ServerSettings) dropForwardProvider(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, byServer := range s.ForwardGrants {
		for sid, loan := range byServer {
			loan.Providers = slices.DeleteFunc(slices.Clone(loan.Providers), func(p string) bool { return p == name })
			if len(loan.Providers) == 0 {
				delete(byServer, sid)
			} else {
				byServer[sid] = loan
			}
		}
		if len(byServer) == 0 {
			delete(s.ForwardGrants, uid)
		}
	}
	_ = s.save()
}

// moveGrantsLocked gives from's grants to into (a merge), keeping both people's providers.
func (s *ServerSettings) moveGrantsLocked(from, into string) {
	for sid, loan := range s.ForwardGrants[from] {
		have := s.ForwardGrants[into][sid]
		if have.Name == "" {
			have.Name = loan.Name
		}
		for _, p := range loan.Providers {
			if !slices.Contains(have.Providers, p) {
				have.Providers = append(have.Providers, p)
			}
		}
		if s.ForwardGrants[into] == nil {
			s.ForwardGrants[into] = map[string]forwardLoan{}
		}
		s.ForwardGrants[into][sid] = have
	}
	delete(s.ForwardGrants, from)
}

// handleGrantEnvelope answers forward_grant, forward_revoke and list_forward_grants. Each only ever
// touches and shows the sender's own grants, and every answer is the whole list.
func handleGrantEnvelope(ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) bool {
	switch env.Type {
	case protocol.EventForwardGrant, protocol.EventForwardRevoke, protocol.EventListForwardGrants:
	default:
		return false
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: msg})
	}
	acc := deps.accessFor(network, peerID)
	changed := false
	switch env.Type {
	case protocol.EventForwardGrant:
		var d protocol.ForwardGrantData
		if json.Unmarshal(env.Data, &d) != nil || d.ServerID == "" || len(d.ServerID) > maxForwardIDLen || d.ProviderName == "" {
			fail("malformed forward_grant payload")
			return true
		}
		cfg, ok := deps.providerStore.Get(d.ProviderName)
		switch {
		case !ok:
			fail(fmt.Sprintf("unknown provider %q", d.ProviderName))
			return true
		case !providerAllowed(deps, acc, d.ProviderName):
			fail(fmt.Sprintf("provider %q isn't allowed for this person", d.ProviderName))
			return true
		case !forwardableKind(cfg.Kind):
			fail(fmt.Sprintf("provider %q can't be used from another server", d.ProviderName))
			return true
		}
		if err := deps.settings.GrantForward(acc.UserID, d.ServerID, d.ServerName, d.ProviderName); err != nil {
			fail(err.Error())
			return true
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.ForwardGrant, Target: d.ProviderName, Outcome: "ok", Detail: map[string]any{"serverId": d.ServerID, "serverName": d.ServerName}})
		changed = true
	case protocol.EventForwardRevoke:
		var d protocol.ForwardRevokeData
		if json.Unmarshal(env.Data, &d) != nil || d.ServerID == "" || d.ProviderName == "" {
			fail("malformed forward_revoke payload")
			return true
		}
		if err := deps.settings.RevokeForward(acc.UserID, d.ServerID, d.ProviderName); err != nil {
			fail(err.Error())
			return true
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.ForwardRevoke, Target: d.ProviderName, Outcome: "ok", Detail: map[string]any{"serverId": d.ServerID}})
		changed = true
	}
	data := protocol.ForwardGrantsData{Grants: deps.settings.ListForwardGrants(acc.UserID)}
	_ = ipc.SendEvent(protocol.EventForwardGrants, "", "", "", data)
	if changed && deps.peers != nil {
		for _, p := range deps.peers.All() {
			if p.ipc != ipc && deps.settings.SameUser(deps.accessFor(p.Network, p.ID).UserID, acc.UserID) {
				_ = p.ipc.SendEvent(protocol.EventForwardGrants, "", "", "", data)
			}
		}
	}
	return true
}
