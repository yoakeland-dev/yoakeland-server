package main

import (
	"encoding/json"
	"slices"
	"sync"
	"time"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

// The server's audit trail (SPEC.md §3.1 list_audit; internal/audit). The
// server-level log lives in <state-dir>/audit/; these are the ways the
// dispatcher and the agents reach it.

// Identify names the device behind a live peer: its name and address now.
// actorOf is who the audit log blames for a peer's actions: the credential it signed in with,
// which the device can't choose, else (local socket) the name it gave itself.
func actorOf(p *Peer) string {
	if p.TokenName != "" {
		return p.TokenName
	}
	return p.Name
}

func (r *PeerRegistry) Identify(id string) (name, ip, network string, ok bool) {
	if r == nil {
		return "", "", "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, found := r.peers[id]; found {
		return actorOf(p), p.IP, p.Network, true
	}
	return "", "", "", false
}

// identifyIPC finds the peer whose connection is ipc, for a handler that was
// not handed its peer id.
func (r *PeerRegistry) identifyIPC(ipc *protocol.IPCStream) (id string) {
	if r == nil || ipc == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.peers {
		if p.ipc == ipc {
			return p.ID
		}
	}
	return ""
}

// record appends e to the audit log, naming the device that caused it: the
// peer peerID (or, if that is empty, the one on ipc). A connection with no
// registered peer (-stdio) is "local".
func (d connDeps) record(ipc *protocol.IPCStream, peerID string, e audit.Event) {
	if d.audit == nil {
		return
	}
	if peerID == "" {
		peerID = d.peers.identifyIPC(ipc)
	}
	if name, ip, network, ok := d.peers.Identify(peerID); ok {
		e.Actor, e.IP, e.Network = name, ip, network
	} else if e.Actor == "" {
		e.Actor, e.Network = "local", "stdio"
	}
	d.audit.Append(e)
}

// actorName is the name of the device behind a request, "local" if it has none.
func (d connDeps) actorName(ipc *protocol.IPCStream, peerID string) string {
	if peerID == "" {
		peerID = d.peers.identifyIPC(ipc)
	}
	if name, _, _, ok := d.peers.Identify(peerID); ok {
		return name
	}
	return "local"
}

// changedKeys lists the top-level keys of a JSON object payload, sorted.
// Settings changes are logged by key, never by value.
func changedKeys(raw json.RawMessage) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// authFailLimiter stops a stranger guessing passwords from also filling the
// audit log: one auth_fail per address per interval, counting what it skipped.
type authFailLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	skip map[string]int
}

const authFailLogEvery = 30 * time.Second

// allow reports whether to log an auth_fail from ip now, and how many were
// skipped since the last one that was logged.
func (l *authFailLimiter) allow(ip string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil || len(l.last) > 1024 {
		l.last, l.skip = map[string]time.Time{}, map[string]int{}
	}
	if t, seen := l.last[ip]; seen && now.Sub(t) < authFailLogEvery {
		l.skip[ip]++
		return false, 0
	}
	n := l.skip[ip]
	l.last[ip], l.skip[ip] = now, 0
	return true, n
}

var authFails authFailLimiter

// recordAuthFail logs a TCP client that was refused: a bad credential, or a pairing code that is
// expired, used, declined or unanswered (reason says which).
func (d connDeps) recordAuthFail(ip, network, reason string) {
	if d.audit == nil {
		return
	}
	ok, skipped := authFails.allow(ip, time.Now())
	if !ok {
		return
	}
	e := audit.Event{Kind: audit.AuthFail, IP: ip, Network: network, Outcome: "deny"}
	detail := map[string]any{}
	if reason != "" {
		detail["reason"] = reason
	}
	if skipped > 0 {
		detail["alsoRejected"] = skipped
	}
	if len(detail) > 0 {
		e.Detail = detail
	}
	d.audit.Append(e)
}

// recordPeer logs a device connecting or leaving.
func (d connDeps) recordPeer(kind string, p *Peer) {
	if d.audit == nil {
		return
	}
	name, ip, network, _ := d.peers.Identify(p.ID)
	if name == "" { // already removed from the roster when it leaves: use what the peer held
		name, ip, network = actorOf(p), p.IP, p.Network
	}
	detail := map[string]any{"deviceType": p.DeviceType, "name": p.Name}
	if p.TokenName != "" {
		detail["credentialName"] = p.TokenName
	}
	d.audit.Append(audit.Event{Kind: kind, Actor: name, IP: ip, Network: network, Outcome: "ok", Detail: detail})
}

// handleListAudit answers list_audit with a page of the log, newest first.
// Only reached past the handshake, so the asker is an authenticated peer.
func handleListAudit(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) {
	var d protocol.ListAuditData
	_ = json.Unmarshal(env.Data, &d)
	out := protocol.AuditListData{RequestID: d.RequestID, Events: []audit.Event{}}
	q := audit.Query{Before: d.Before, Limit: d.Limit, Kinds: d.Kinds, Session: d.SessionID, Project: d.Project}
	if d.Since != "" {
		t, err := time.Parse(time.RFC3339, d.Since)
		if err != nil {
			out.Error = "since must be an RFC 3339 time"
			_ = ipc.SendEvent(protocol.EventAuditList, env.SessionID, env.TurnID, "", out)
			return
		}
		q.Since = t
	}
	events, more, err := deps.audit.Read(q)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Events, out.More = events, more
	}
	_ = ipc.SendEvent(protocol.EventAuditList, env.SessionID, env.TurnID, "", out)
}
