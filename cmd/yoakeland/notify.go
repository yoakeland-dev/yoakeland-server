package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/fsutil"
	"yoakeland/internal/protocol"
)

const (
	// maxNotifications and notificationMaxAge bound what the feed keeps (and
	// notifications.jsonl holds): the newest 200 from the last 14 days.
	maxNotifications   = 200
	notificationMaxAge = 14 * 24 * time.Hour

	// coalesceWindow: a repeat of an entry this recent replaces it (count+1)
	// instead of adding a line. foldWindow: a repeat this recent is the same
	// incident reported twice (an error and the failed turn that carried it),
	// so the count stays.
	coalesceWindow = time.Minute
	foldWindow     = 5 * time.Second
)

// Priorities (protocol.Notification.Priority).
const (
	prioUrgent = "urgent"
	prioNormal = "normal"
	prioQuiet  = "quiet"
)

// Kinds of notification, each with its priority. Asks are urgent because
// the agent is blocked on you; a finished turn is only normal, and only worth
// raising when it ran long in a chat nobody is looking at.
const (
	kindTurnDone   = "turn_done"
	kindTurnCutOff = "turn_cut_off"
	kindBudget     = "budget_exceeded"
	kindNoProvider = "no_provider"
	kindTurnError  = "turn_error"
	kindPermission = "permission_request"
	kindQuestion   = "ask_user_question_requested"
	kindPeerJoined = "peer_joined"
	kindPeerLeft   = "peer_left"
)

var kindPriority = map[string]string{
	kindTurnDone: prioNormal, kindTurnCutOff: prioNormal,
	kindBudget: prioUrgent, kindNoProvider: prioUrgent, kindTurnError: prioUrgent,
	kindPermission: prioUrgent, kindQuestion: prioUrgent,
	kindPeerJoined: prioQuiet, kindPeerLeft: prioQuiet,
}

// askKind: the entry mirrors a live pending ask, so it is never persisted
// (asks don't survive a restart) and Reconcile retracts it when the ask is
// answered or cancelled.
func askKind(kind string) bool { return kind == kindPermission || kind == kindQuestion }

// defaultNotifyPrefs: everything that needs you, plus long finished turns;
// device presence is opt-in.
func defaultNotifyPrefs() protocol.NotifyPrefsData {
	return protocol.NotifyPrefsData{Urgent: true, Normal: true, MinTurnSec: 30, Muted: []string{}}
}

// allows reports whether n's tier is on and its kind isn't muted.
func allowsNotification(p protocol.NotifyPrefsData, n protocol.Notification) bool {
	switch n.Priority {
	case prioUrgent:
		if !p.Urgent {
			return false
		}
	case prioNormal:
		if !p.Normal {
			return false
		}
	default:
		if !p.Quiet {
			return false
		}
	}
	return !slices.Contains(p.Muted, n.Kind)
}

// NotificationFeed is the server-wide notification center (SPEC.md §3.2
// notification): raised by whatever event deserves a human's attention,
// filtered by the owner's prefs and by who is looking where, broadcast to
// every peer, and kept in <state-dir>/notifications.jsonl so it outlives a
// restart. Read state lives here too, so it follows you across devices.
// A nil *NotificationFeed is a no-op, so tests' bare connDeps keep working.
type NotificationFeed struct {
	mu    sync.Mutex
	items []protocol.Notification
	peers *PeerRegistry

	// prefs reads the current policy; nil means the defaults.
	prefs func() protocol.NotifyPrefsData
	// path is the jsonl file, "" for memory only.
	path string
	now  func() time.Time
	// asked holds the toolCallIds Reconcile already turned into (or
	// filtered out of) a notification, so a dismissed or muted ask isn't
	// raised again on the next pending_asks change. Pruned as asks end.
	asked map[string]bool
}

func NewNotificationFeed(peers *PeerRegistry) *NotificationFeed {
	return &NotificationFeed{peers: peers, now: time.Now, asked: map[string]bool{}}
}

// OpenNotificationFeed is NewNotificationFeed backed by path (created on
// first write); what it finds there is loaded, minus anything past the
// retention limits.
func OpenNotificationFeed(peers *PeerRegistry, path string, prefs func() protocol.NotifyPrefsData) *NotificationFeed {
	f := NewNotificationFeed(peers)
	f.prefs, f.path = prefs, path
	f.load()
	return f
}

func (f *NotificationFeed) currentPrefs() protocol.NotifyPrefsData {
	if f.prefs == nil {
		return defaultNotifyPrefs()
	}
	return f.prefs()
}

// notifyKey says which entries are "the same thing": same kind in the same
// chat, or for chat-less kinds (a device connecting) the same title.
func notifyKey(n protocol.Notification) string {
	if n.SessionID == "" {
		return n.Kind + "||" + n.Title
	}
	return n.Kind + "|" + n.SessionID
}

// Push filters n through the prefs and presence, stamps it, folds it into a
// recent identical entry if there is one, records it, and broadcasts it to
// every peer except exceptPeerID ("" excludes nobody).
func (f *NotificationFeed) Push(exceptPeerID string, n protocol.Notification) {
	if f == nil {
		return
	}
	if n.Priority == "" {
		n.Priority = kindPriority[n.Kind]
	}
	if !allowsNotification(f.currentPrefs(), n) {
		return
	}
	// Someone is reading this chat right now: a "finished" ping is redundant,
	// and anything else is born read so it doesn't badge.
	viewing := f.peers.Viewing(n.SessionID)
	if viewing && n.Priority == prioNormal {
		return
	}
	n.Read = viewing
	n.Attended = n.Priority != prioQuiet && f.peers.Attended()
	n.Count = 1
	now := f.now().UTC()
	n.CreatedAt = now

	f.mu.Lock()
	key := notifyKey(n)
	merged := -1
	for i := len(f.items) - 1; i >= 0; i-- {
		if notifyKey(f.items[i]) == key && now.Sub(f.items[i].CreatedAt) < coalesceWindow {
			merged = i
			break
		}
	}
	rewrite := false
	if merged >= 0 {
		old := f.items[merged]
		n.ID = old.ID
		n.Count = max(old.Count, 1)
		if now.Sub(old.CreatedAt) >= foldWindow || old.ToolCallID != n.ToolCallID {
			n.Count++ // not the same incident reported twice
		}
		f.items = append(f.items[:merged:merged], f.items[merged+1:]...)
		rewrite = true
	} else {
		n.ID = randomID()
	}
	f.items = append(f.items, n)
	if f.trimLocked(now) {
		rewrite = true
	}
	f.saveLocked(rewrite, n)
	f.mu.Unlock()
	f.peers.BroadcastEvent(exceptPeerID, protocol.EventNotification, "", n)
}

// trimLocked drops what is past the age or count limit, reporting whether it did.
func (f *NotificationFeed) trimLocked(now time.Time) bool {
	before := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(n protocol.Notification) bool { return now.Sub(n.CreatedAt) > notificationMaxAge })
	if len(f.items) > maxNotifications {
		f.items = append([]protocol.Notification(nil), f.items[len(f.items)-maxNotifications:]...)
	}
	return len(f.items) != before
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
		f.items = slices.DeleteFunc(f.items, func(n protocol.Notification) bool { return n.ID == id })
	}
	f.saveLocked(true, protocol.Notification{})
	f.mu.Unlock()
	f.peers.BroadcastEvent("", protocol.EventNotificationDismissed, "", protocol.DismissNotificationData{ID: id})
}

// MarkRead marks the listed notifications read (all of them for no ids) and
// tells every peer which ones changed; nothing changed, nothing sent.
func (f *NotificationFeed) MarkRead(ids ...string) {
	f.markRead(func(n protocol.Notification) bool { return len(ids) == 0 || slices.Contains(ids, n.ID) })
}

// MarkSessionRead marks everything about sessionID read: someone is looking at it.
func (f *NotificationFeed) MarkSessionRead(sessionID string) {
	if sessionID != "" {
		f.markRead(func(n protocol.Notification) bool { return n.SessionID == sessionID })
	}
}

func (f *NotificationFeed) markRead(match func(protocol.Notification) bool) {
	if f == nil {
		return
	}
	var changed []string
	f.mu.Lock()
	for i := range f.items {
		if !f.items[i].Read && match(f.items[i]) {
			f.items[i].Read = true
			changed = append(changed, f.items[i].ID)
		}
	}
	if len(changed) > 0 {
		f.saveLocked(true, protocol.Notification{})
	}
	f.mu.Unlock()
	if len(changed) > 0 {
		f.peers.BroadcastEvent("", protocol.EventNotificationsRead, "", protocol.MarkNotificationsReadData{IDs: changed})
	}
}

// Reconcile keeps the ask notifications in step with the server's pending
// asks: a new ask raises "permission needed"/"question" (folded per chat), and
// an entry whose chat has no ask left — answered, cancelled, session gone —
// is retracted on every device. label names a chat for the body.
func (f *NotificationFeed) Reconcile(pending []protocol.PendingAsk, label func(projectPath, sessionID string) string) {
	if f == nil {
		return
	}
	live := map[string]bool{}    // toolCallId
	waiting := map[string]bool{} // notifyKey of a chat with an ask open
	var fresh []protocol.PendingAsk
	f.mu.Lock()
	for _, a := range pending {
		live[a.ToolCallID] = true
		waiting[askKindOf(a)+"|"+a.SessionID] = true
		if !f.asked[a.ToolCallID] {
			f.asked[a.ToolCallID] = true
			fresh = append(fresh, a)
		}
	}
	for id := range f.asked {
		if !live[id] {
			delete(f.asked, id)
		}
	}
	var gone []string
	for _, n := range f.items {
		if askKind(n.Kind) && !waiting[n.Kind+"|"+n.SessionID] {
			gone = append(gone, n.ID)
		}
	}
	f.mu.Unlock()
	for _, id := range gone {
		f.Dismiss(id)
	}
	for _, a := range fresh {
		n := protocol.Notification{Kind: askKindOf(a), Level: "warning", Title: "Permission needed", SessionID: a.SessionID, ProjectPath: a.ProjectPath, ToolCallID: a.ToolCallID}
		body := ""
		switch {
		case a.PermissionRequestData != nil:
			n.Title, body = "Permission needed: "+a.ToolName, a.Description
		case a.AskUserQuestionRequestedData != nil:
			n.Level, n.Title, body = "info", "The agent has a question", a.Question
		}
		n.Body = labelledBody(label(a.ProjectPath, a.SessionID), body)
		f.Push("", n)
	}
}

func askKindOf(a protocol.PendingAsk) string {
	if a.Kind == "question" {
		return kindQuestion
	}
	return kindPermission
}

func labelledBody(label, body string) string {
	if body == "" {
		return label
	}
	return label + ": " + body
}

// saveLocked writes to disk: a plain push appends its line; anything else
// (dismiss, read, fold, trim) rewrites the file atomically. Failures are
// logged, not fatal: the feed still works in memory.
func (f *NotificationFeed) saveLocked(rewrite bool, added protocol.Notification) {
	if f.path == "" {
		return
	}
	if !rewrite {
		if askKind(added.Kind) {
			return
		}
		line, err := json.Marshal(added)
		if err == nil {
			err = appendFile(f.path, append(line, '\n'))
		}
		if err != nil {
			logWarn("notifications: append to %s: %v", f.path, err)
		}
		return
	}
	var buf bytes.Buffer
	for _, n := range f.items {
		if !askKind(n.Kind) {
			line, _ := json.Marshal(n)
			buf.Write(append(line, '\n'))
		}
	}
	if err := fsutil.WriteFileAtomic(f.path, buf.Bytes(), 0o600); err != nil {
		logWarn("notifications: rewrite %s: %v", f.path, err)
	}
}

// load reads the file back, keeping the newest maxNotifications entries
// younger than notificationMaxAge. Unreadable lines are skipped; if anything
// was dropped the file is rewritten so it doesn't grow without bound.
func (f *NotificationFeed) load() {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logWarn("notifications: read %s: %v", f.path, err)
		}
		return
	}
	bad := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var n protocol.Notification
		if json.Unmarshal(line, &n) != nil || n.ID == "" || askKind(n.Kind) {
			bad++
			continue
		}
		n.Count = max(n.Count, 1)
		f.items = append(f.items, n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trimLocked(f.now().UTC()) || bad > 0 {
		f.saveLocked(true, protocol.Notification{})
	}
}

func appendFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// sessionTap turns a project's session events into notifications — the
// ones a person not currently looking at that chat would want to hear
// about. Installed on each project's agent.Registry (see ProjectRegistry.Get).
// Asks are not handled here: Reconcile mirrors the pending-ask list instead,
// so they are retracted when answered.
func (f *NotificationFeed) sessionTap(projectRoot string, sessStore agent.SessionStore) func(protocol.EventType, string, string, any) {
	return func(evt protocol.EventType, sessionID, _ string, payload any) {
		n := protocol.Notification{SessionID: sessionID, ProjectPath: projectRoot}
		switch evt {
		case protocol.EventTurnCompleted:
			d, ok := payload.(protocol.TurnCompletedData)
			if !ok {
				return
			}
			switch d.FinishReason {
			case "stop":
				if d.DurationMs < int64(f.currentPrefs().MinTurnSec)*1000 {
					return
				}
				n.Kind, n.Level, n.Title = kindTurnDone, "success", "Response ready"
			case "length":
				n.Kind, n.Level, n.Title = kindTurnCutOff, "warning", "Response cut off at the output limit"
			case "budget_exceeded":
				n.Kind, n.Level, n.Title = kindBudget, "error", "Session spend cap reached"
			case "no_provider":
				n.Kind, n.Level, n.Title = kindNoProvider, "error", "No provider configured"
			default: // "cancelled": the user did that themselves
				return
			}
		case protocol.EventError:
			d, ok := payload.(protocol.ErrorData)
			if !ok {
				return
			}
			n.Kind, n.Level, n.Title, n.Body = kindTurnError, "error", "Turn error", d.Message
		default:
			return
		}
		n.Body = labelledBody(sessionLabel(sessStore, sessionID), n.Body)
		f.Push("", n)
	}
}

// turnFailed reports a turn that returned an error to its dispatcher
// instead of through the session stream (provider failures etc.). Same kind
// as an error event: when both fire for one failure they fold into one entry.
func (f *NotificationFeed) turnFailed(pc *ProjectContext, sessionID string, err error) {
	if f == nil {
		return
	}
	f.Push("", protocol.Notification{
		Level:       "error",
		Kind:        kindTurnError,
		Title:       "Turn error",
		Body:        labelledBody(sessionLabel(pc.SessStore, sessionID), err.Error()),
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
	n.notify.Push(p.ID, protocol.Notification{Level: "info", Kind: kindPeerJoined, Title: p.Name + " connected", Body: p.IP})
}

func (n *presenceNotes) Left(p *Peer) {
	if n == nil {
		return
	}
	note := protocol.Notification{Level: "info", Kind: kindPeerLeft, Title: p.Name + " disconnected", Body: p.IP}
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
