package main

import (
	"fmt"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// maxNotifications caps the replay buffer list_notifications serves. Older
// entries fall off the front; a client that wants a longer history keeps
// its own copy of what it was pushed live.
const maxNotifications = 100

// NotificationFeed is the server-wide notification center (SPEC.md §3.2
// notification): raised by whatever event deserves a human's attention,
// broadcast to every peer, and replayable for a client that connects later.
// In memory only. ponytail: gone on restart, persist to -state-dir if a
// notification ever needs to outlive the process.
// A nil *NotificationFeed is a no-op, so tests' bare connDeps keep working.
type NotificationFeed struct {
	mu    sync.Mutex
	items []protocol.Notification
	peers *PeerRegistry
}

func NewNotificationFeed(peers *PeerRegistry) *NotificationFeed {
	return &NotificationFeed{peers: peers}
}

// Push stamps n with an ID/time, records it, and broadcasts it to every
// peer except exceptPeerID ("" excludes nobody).
func (f *NotificationFeed) Push(exceptPeerID string, n protocol.Notification) {
	if f == nil {
		return
	}
	n.ID = randomID()
	n.CreatedAt = time.Now().UTC()
	f.mu.Lock()
	f.items = append(f.items, n)
	if len(f.items) > maxNotifications {
		f.items = append([]protocol.Notification(nil), f.items[len(f.items)-maxNotifications:]...)
	}
	f.mu.Unlock()
	f.peers.BroadcastEvent(exceptPeerID, protocol.EventNotification, "", n)
}

// List returns the buffered notifications, oldest first.
func (f *NotificationFeed) List() []protocol.Notification {
	if f == nil {
		return []protocol.Notification{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Notification{}, f.items...)
}

// Dismiss drops one notification (or all, for id == "") and tells every
// peer, so clearing it on one device clears it everywhere.
func (f *NotificationFeed) Dismiss(id string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if id == "" {
		f.items = nil
	} else {
		for i, n := range f.items {
			if n.ID == id {
				f.items = append(f.items[:i:i], f.items[i+1:]...)
				break
			}
		}
	}
	f.mu.Unlock()
	f.peers.BroadcastEvent("", protocol.EventNotificationDismissed, "", protocol.DismissNotificationData{ID: id})
}

// sessionTap turns a project's session events into notifications — the
// ones a person not currently looking at that chat would want to hear
// about. Installed on each project's agent.Registry (see ProjectRegistry.Get).
func (f *NotificationFeed) sessionTap(projectRoot string, sessStore agent.SessionStore) func(protocol.EventType, string, string, any) {
	return func(evt protocol.EventType, sessionID, _ string, payload any) {
		n := protocol.Notification{Kind: string(evt), SessionID: sessionID, ProjectPath: projectRoot}
		switch evt {
		case protocol.EventTurnCompleted:
			d, ok := payload.(protocol.TurnCompletedData)
			if !ok {
				return
			}
			switch d.FinishReason {
			case "stop":
				n.Level, n.Title = "success", "Response ready"
			case "length":
				n.Level, n.Title = "warning", "Response cut off at the output limit"
			case "budget_exceeded":
				n.Level, n.Title = "error", "Session spend cap reached"
			case "no_provider":
				n.Level, n.Title = "error", "No provider configured"
			default: // "cancelled": the user did that themselves
				return
			}
		case protocol.EventPermissionRequest:
			d, ok := payload.(protocol.PermissionRequestData)
			if !ok {
				return
			}
			n.Level, n.Title, n.Body = "warning", "Permission needed: "+d.ToolName, d.Description
		case protocol.EventAskUserQuestionRequested:
			d, ok := payload.(protocol.AskUserQuestionRequestedData)
			if !ok {
				return
			}
			n.Level, n.Title, n.Body = "info", "The agent has a question", d.Question
		case protocol.EventError:
			d, ok := payload.(protocol.ErrorData)
			if !ok {
				return
			}
			n.Level, n.Title, n.Body = "error", "Turn error", d.Message
		default:
			return
		}
		if n.Body == "" {
			n.Body = sessionLabel(sessStore, sessionID)
		} else {
			n.Body = sessionLabel(sessStore, sessionID) + ": " + n.Body
		}
		f.Push("", n)
	}
}

// turnFailed reports a turn that returned an error to its dispatcher
// instead of through the session stream (provider failures etc.).
func (f *NotificationFeed) turnFailed(pc *ProjectContext, sessionID string, err error) {
	if f == nil {
		return
	}
	f.Push("", protocol.Notification{
		Level:       "error",
		Kind:        "turn_failed",
		Title:       "Turn failed",
		Body:        fmt.Sprintf("%s: %v", sessionLabel(pc.SessStore, sessionID), err),
		SessionID:   sessionID,
		ProjectPath: pc.Root,
	})
}

func sessionLabel(store agent.SessionStore, sessionID string) string {
	if state, err := store.Load(sessionID); err == nil && state.Title != "" {
		return state.Title
	}
	return "Untitled chat"
}

// presenceGrace is how long a remote device may be gone before its
// "disconnected" notification is posted — a reconnect inside it (wifi
// blip, laptop lid, app restart) posts nothing at all.
const presenceGrace = 30 * time.Second

// presenceNotes turns remote peers coming and going into notifications,
// holding each "disconnected" back for presenceGrace so a device that's
// only reconnecting doesn't spam a leave/join pair. Devices are matched by
// DeviceID; a client that doesn't send one gets both notifications at once.
type presenceNotes struct {
	notify *NotificationFeed
	grace  time.Duration

	mu      sync.Mutex
	leaving map[string]*time.Timer // DeviceID -> its pending "disconnected"
}

func newPresenceNotes(notify *NotificationFeed) *presenceNotes {
	return &presenceNotes{notify: notify, grace: presenceGrace, leaving: map[string]*time.Timer{}}
}

func (n *presenceNotes) Joined(p *Peer) {
	if n == nil {
		return
	}
	if p.DeviceID != "" {
		n.mu.Lock()
		t := n.leaving[p.DeviceID]
		back := t != nil && t.Stop()
		delete(n.leaving, p.DeviceID)
		n.mu.Unlock()
		if back {
			return
		}
	}
	n.notify.Push(p.ID, protocol.Notification{Level: "info", Kind: "peer_joined", Title: p.Name + " connected", Body: p.IP})
}

func (n *presenceNotes) Left(p *Peer) {
	if n == nil {
		return
	}
	note := protocol.Notification{Level: "info", Kind: "peer_left", Title: p.Name + " disconnected", Body: p.IP}
	if p.DeviceID == "" {
		n.notify.Push(p.ID, note)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if old := n.leaving[p.DeviceID]; old != nil {
		old.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(n.grace, func() {
		n.mu.Lock()
		if n.leaving[p.DeviceID] == t {
			delete(n.leaving, p.DeviceID)
		}
		n.mu.Unlock()
		n.notify.Push(p.ID, note)
	})
	n.leaving[p.DeviceID] = t
}
