package main

import "yoakeland/internal/protocol"

// canSee reports whether p may be told about events of the chat sessionID. The local socket sees
// everything; an event with no chat is for everyone.
func (r *PeerRegistry) canSee(p *Peer, sessionID string) bool {
	if sessionID == "" || p.Network != "tcp" {
		return true
	}
	return r.Sees != nil && r.Sees("tcp", p.TokenName, sessionID)
}

// forPeer is data as p may see it. ok is false when the event is about a chat p may not see.
// Payloads that carry chats inside them (a notification, the list of unanswered asks, the terminal
// index) are cut down to the chats p may see, so a server-wide push can't carry one person's chat
// to another. Every server-wide send goes through here: BroadcastEvent and SendTo.
func (r *PeerRegistry) forPeer(p *Peer, sessionID string, data any) (out any, ok bool) {
	if !r.canSee(p, sessionID) {
		return nil, false
	}
	switch d := data.(type) {
	case protocol.Notification:
		return d, r.canSee(p, d.SessionID)
	case protocol.PendingAsksData:
		asks := make([]protocol.PendingAsk, 0, len(d.Asks))
		for _, a := range d.Asks {
			if r.canSee(p, a.SessionID) {
				asks = append(asks, a)
			}
		}
		d.Asks = asks
		return d, true
	case protocol.TerminalIndexData:
		terms := make([]protocol.TerminalSummary, 0, len(d.Terminals))
		for _, t := range d.Terminals {
			if r.canSee(p, t.SessionID) {
				terms = append(terms, t)
			}
		}
		d.Terminals = terms
		return d, true
	case protocol.PingData:
		d.Status = statusForPeer(r, p, d.Status)
		return d, true
	case protocol.NotificationListData:
		list := make([]protocol.Notification, 0, len(d.Notifications))
		for _, n := range d.Notifications {
			if r.canSee(p, n.SessionID) {
				list = append(list, n)
			}
		}
		d.Notifications = list
		return d, true
	}
	return data, true
}

// SendTo sends one event to p, as p may see it.
func (r *PeerRegistry) SendTo(p *Peer, eventType protocol.EventType, sessionID string, data any) {
	if data, ok := r.forPeer(p, sessionID, data); ok {
		_ = p.ipc.SendEvent(eventType, sessionID, "", "", data)
	}
}

// statusForPeer is a server status as p may read it: the server-wide spend totals and the owner's
// subscription usage are for Trusted and up, so a Limited person's status bar has neither.
func statusForPeer(r *PeerRegistry, p *Peer, st *protocol.ServerStatus) *protocol.ServerStatus {
	if st == nil || tierRank(tierOrFull(p)) >= tierRank(TierTrusted) {
		return st
	}
	cp := *st
	cp.Spend, cp.RateLimits = nil, nil
	return &cp
}
