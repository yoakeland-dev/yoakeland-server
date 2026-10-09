package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// testPrefs turns every tier on and the duration threshold off, so a test
// only meets the filter it is about.
func testPrefs() protocol.NotifyPrefsData {
	return protocol.NotifyPrefsData{Urgent: true, Normal: true, Quiet: true, Muted: []string{}}
}

// testFeed is an in-memory feed on a manual clock.
func testFeed(t *testing.T) (*NotificationFeed, *time.Time) {
	t.Helper()
	f := NewNotificationFeed(NewPeerRegistry())
	f.prefs = testPrefs
	clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return clock }
	return f, &clock
}

// present marks p as showing sessionID in a focused window.
func present(p *Peer, sessionID string, focused bool) {
	p.presence = presenceState{sessions: []string{sessionID}, focused: focused, visible: true, at: time.Now()}
}

func TestNotificationFeed_SessionTap(t *testing.T) {
	f, _ := testFeed(t)
	f.prefs = func() protocol.NotifyPrefsData { p := testPrefs(); p.MinTurnSec = 30; return p }
	store, err := agent.NewDiskSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tap := f.sessionTap("/proj", store)
	tap(protocol.EventModelDelta, "s1", "t1", protocol.ModelDeltaData{})
	tap(protocol.EventTurnCompleted, "s1", "t1", protocol.TurnCompletedData{FinishReason: "cancelled"})
	tap(protocol.EventTurnCompleted, "s1", "t1", protocol.TurnCompletedData{FinishReason: "stop", DurationMs: 5_000}) // too quick to matter
	tap(protocol.EventPermissionRequest, "s1", "t1", protocol.PermissionRequestData{ToolName: "bash"})                // Reconcile's job
	tap(protocol.EventTurnCompleted, "s2", "t2", protocol.TurnCompletedData{FinishReason: "stop", DurationMs: 45_000})
	tap(protocol.EventTurnCompleted, "s3", "t3", protocol.TurnCompletedData{FinishReason: "budget_exceeded"})

	got := f.List()
	if len(got) != 2 {
		t.Fatalf("want the long turn and the budget stop, got %+v", got)
	}
	if got[0].Kind != kindTurnDone || got[0].Priority != prioNormal || got[0].Level != "success" || got[0].ProjectPath != "/proj" || got[0].Count != 1 {
		t.Fatalf("turn done: %+v", got[0])
	}
	if got[1].Kind != kindBudget || got[1].Priority != prioUrgent {
		t.Fatalf("budget: %+v", got[1])
	}
	f.Dismiss(got[0].ID)
	if l := f.List(); len(l) != 1 || l[0].ID != got[1].ID {
		t.Fatalf("dismiss one: %+v", l)
	}
	f.Dismiss("")
	if len(f.List()) != 0 {
		t.Fatal("dismiss all")
	}
}

func TestNotificationFeed_PrefsFilter(t *testing.T) {
	f, _ := testFeed(t)
	p := testPrefs()
	f.prefs = func() protocol.NotifyPrefsData { return p }
	push := func(kind, session string) {
		f.Push("", protocol.Notification{Kind: kind, Title: kind, SessionID: session})
	}

	p.Quiet = false
	push(kindPeerJoined, "")
	p.Normal = false
	push(kindTurnDone, "a")
	if len(f.List()) != 0 {
		t.Fatalf("switched-off tiers still raised: %+v", f.List())
	}
	p.Normal, p.Muted = true, []string{kindTurnCutOff}
	push(kindTurnCutOff, "a")
	push(kindTurnDone, "a")
	if got := kinds(f); got != kindTurnDone {
		t.Fatalf("muted kind got through: %s", got)
	}

	// The defaults are what the owner asked for: quiet off, the rest on, 30s.
	d := defaultNotifyPrefs()
	if !d.Urgent || !d.Normal || d.Quiet || d.MinTurnSec != 30 {
		t.Fatalf("defaults: %+v", d)
	}
}

func TestNotificationFeed_Coalesce(t *testing.T) {
	f, clock := testFeed(t)
	push := func(body string) {
		f.Push("", protocol.Notification{Kind: kindTurnError, Title: "Turn error", Body: body, SessionID: "s1"})
	}

	push("provider 500")
	push("Chat: provider 500 (from the failed turn)") // same incident, reported twice
	got := f.List()
	if len(got) != 1 || got[0].Count != 1 || !strings.HasPrefix(got[0].Body, "Chat:") {
		t.Fatalf("an error and its failed turn should fold into one: %+v", got)
	}
	id := got[0].ID

	*clock = clock.Add(20 * time.Second)
	push("again")
	*clock = clock.Add(20 * time.Second)
	push("and again")
	got = f.List()
	if len(got) != 1 || got[0].Count != 3 || got[0].ID != id || got[0].Body != "and again" {
		t.Fatalf("repeats within a minute should count up: %+v", got)
	}

	f.Push("", protocol.Notification{Kind: kindTurnError, Title: "Turn error", SessionID: "s2"}) // another chat
	*clock = clock.Add(2 * time.Minute)
	push("much later")
	if got := f.List(); len(got) != 3 {
		t.Fatalf("other chats and stale entries stay separate: %+v", got)
	}
}

func TestNotificationFeed_PresenceSuppression(t *testing.T) {
	f, _ := testFeed(t)
	desk, deskOut := fakeIPCPeer(t, f.peers, "desk", "desktop")
	phone, phoneOut := fakeIPCPeer(t, f.peers, "phone", "web")

	// Nobody around: normal and urgent both land unread and unattended.
	f.Push("", protocol.Notification{Kind: kindTurnDone, Title: "done", SessionID: "s1"})
	if l := f.List(); len(l) != 1 || l[0].Read || l[0].Attended {
		t.Fatalf("idle: %+v", l)
	}

	// The desk is focused on s1: a "response ready" for s1 is dropped, an
	// error for it is kept but born read, and one for s2 is raised as
	// attended (the phone should not buzz on top of it).
	present(desk, "s1", true)
	f.Push("", protocol.Notification{Kind: kindTurnDone, Title: "done", SessionID: "s1"})
	f.Push("", protocol.Notification{Kind: kindTurnError, Title: "err", SessionID: "s1"})
	f.Push("", protocol.Notification{Kind: kindTurnDone, Title: "done", SessionID: "s2"})
	l := f.List()
	if len(l) != 3 {
		t.Fatalf("want the idle one, s1's error, and s2's: %+v", l)
	}
	if !l[1].Read || l[1].Kind != kindTurnError {
		t.Fatalf("error for the viewed chat should be born read: %+v", l[1])
	}
	if l[2].Read || !l[2].Attended {
		t.Fatalf("other chat's entry: %+v", l[2])
	}

	// An unfocused window, or a stale heartbeat, counts for nothing.
	present(desk, "s1", false)
	desk.presence.focused = false
	phone.presence = presenceState{sessions: []string{"s3"}, focused: true, visible: true, at: time.Now().Add(-presenceTTL - time.Second)}
	if f.peers.Viewing("s1") || f.peers.Viewing("s3") || f.peers.Attended() {
		t.Fatal("unfocused / stale presence counted")
	}
	_, _ = deskOut, phoneOut
}

func TestNotificationFeed_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.jsonl")
	f := OpenNotificationFeed(NewPeerRegistry(), path, testPrefs)
	clock := time.Now()
	f.now = func() time.Time { return clock }

	f.Push("", protocol.Notification{Kind: kindTurnDone, Title: "one", SessionID: "a"})
	f.Push("", protocol.Notification{Kind: kindBudget, Title: "two", SessionID: "b"})
	f.Push("", protocol.Notification{Kind: kindPermission, Title: "ask", SessionID: "c"}) // mirrors a live ask
	list := f.List()
	f.MarkRead(list[1].ID)
	f.Dismiss(list[0].ID)

	again := OpenNotificationFeed(NewPeerRegistry(), path, testPrefs)
	got := again.List()
	if len(got) != 1 || got[0].Title != "two" || !got[0].Read {
		t.Fatalf("reloaded: %+v (want one read entry, no ask, no dismissed one)", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func TestNotificationFeed_Retention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.jsonl")
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	write := func(n protocol.Notification) {
		line, _ := json.Marshal(n)
		b.Write(append(line, '\n'))
	}
	write(protocol.Notification{ID: "old", Kind: kindBudget, CreatedAt: now.Add(-15 * 24 * time.Hour)})
	b.WriteString("{not json\n")
	for i := 0; i < maxNotifications+10; i++ {
		write(protocol.Notification{ID: randomID(), Kind: kindBudget, Title: "n", CreatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	f := NewNotificationFeed(NewPeerRegistry())
	f.path, f.now = path, func() time.Time { return now }
	f.load()
	got := f.List()
	if len(got) != maxNotifications {
		t.Fatalf("kept %d, want the newest %d", len(got), maxNotifications)
	}
	for _, n := range got {
		if n.ID == "old" {
			t.Fatal("kept an entry past 14 days")
		}
	}
	// Load rewrote the file down to what it kept.
	if data, _ := os.ReadFile(path); bytes.Count(data, []byte("\n")) != maxNotifications || bytes.Contains(data, []byte("{not json")) {
		t.Fatalf("file not compacted: %d lines", bytes.Count(data, []byte("\n")))
	}
}

func asksFor(session, project string, ids ...string) []protocol.PendingAsk {
	var out []protocol.PendingAsk
	for _, id := range ids {
		out = append(out, protocol.PendingAsk{Kind: "permission", SessionID: session, ProjectPath: project, ToolCallID: id,
			PermissionRequestData: &protocol.PermissionRequestData{ToolName: "bash", Description: "rm -rf build"}})
	}
	return out
}

func TestNotificationFeed_ReconcileRetractsAnsweredAsks(t *testing.T) {
	f, _ := testFeed(t)
	_, out := fakeIPCPeer(t, f.peers, "desk", "desktop")
	label := func(_, id string) string { return "Chat " + id }

	f.Reconcile(asksFor("s1", "/p", "tc1"), label)
	got := f.List()
	if len(got) != 1 || got[0].Kind != kindPermission || got[0].Title != "Permission needed: bash" || got[0].Body != "Chat s1: rm -rf build" || got[0].ToolCallID != "tc1" {
		t.Fatalf("raised: %+v", got)
	}

	// A second ask in the same chat folds in; the first answer leaves the entry.
	f.Reconcile(asksFor("s1", "/p", "tc1", "tc2"), label)
	if got := f.List(); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("second ask should fold: %+v", got)
	}
	f.Reconcile(asksFor("s1", "/p", "tc2"), label)
	if len(f.List()) != 1 {
		t.Fatal("retracted while an ask was still open")
	}

	// Answered (or the turn cancelled): gone, and every peer is told.
	id := f.List()[0].ID
	f.Reconcile(nil, label)
	if len(f.List()) != 0 {
		t.Fatal("answered ask not retracted")
	}
	if !strings.Contains(out.String(), `"type":"notification_dismissed"`) || !strings.Contains(out.String(), id) {
		t.Fatalf("peers not told:\n%s", out.String())
	}

	// A dismissed ask is not raised again by the next pending_asks change.
	f.Reconcile(asksFor("s2", "/p", "tc9"), label)
	f.Dismiss("")
	f.Reconcile(asksFor("s2", "/p", "tc9"), label)
	if len(f.List()) != 0 {
		t.Fatalf("dismissed ask came back: %+v", f.List())
	}
}

func TestNotificationFeed_ReconcileQuestion(t *testing.T) {
	f, _ := testFeed(t)
	f.Reconcile([]protocol.PendingAsk{{Kind: "question", SessionID: "s1", ToolCallID: "q1",
		AskUserQuestionRequestedData: &protocol.AskUserQuestionRequestedData{Question: "Which db?"}}}, func(_, _ string) string { return "Chat" })
	got := f.List()
	if len(got) != 1 || got[0].Kind != kindQuestion || got[0].Body != "Chat: Which db?" || got[0].Priority != prioUrgent {
		t.Fatalf("%+v", got)
	}
}

func TestNotificationFeed_MarkReadBroadcastsOnlyChanges(t *testing.T) {
	f, _ := testFeed(t)
	_, out := fakeIPCPeer(t, f.peers, "desk", "desktop")
	f.Push("", protocol.Notification{Kind: kindBudget, Title: "a", SessionID: "s1"})
	f.Push("", protocol.Notification{Kind: kindBudget, Title: "b", SessionID: "s2"})
	out.Reset()

	f.MarkSessionRead("s1")
	if n := strings.Count(out.String(), "notifications_read"); n != 1 {
		t.Fatalf("want one notifications_read, got %d:\n%s", n, out.String())
	}
	out.Reset()
	f.MarkSessionRead("s1") // nothing left to change
	if out.Len() != 0 {
		t.Fatalf("no-op mark broadcast: %s", out.String())
	}
	f.MarkRead()
	for _, n := range f.List() {
		if !n.Read {
			t.Fatalf("mark all left %+v", n)
		}
	}
}

// One device marking read, or simply opening the chat, clears the badge on
// the others: it goes through the server, which tells every peer.
func TestServeConnection_NotificationReadSyncsAcrossPeers(t *testing.T) {
	deps := newTestDeps(t)
	deps.notify = NewNotificationFeed(deps.peers)
	deps.notify.prefs = testPrefs
	_, laptopOut := fakeIPCPeer(t, deps.peers, "laptop", "desktop")
	phone, phoneOut := fakeIPCPeer(t, deps.peers, "phone", "web")

	deps.notify.Push("", protocol.Notification{Kind: kindBudget, Title: "cap", SessionID: "s1"})
	deps.notify.Push("", protocol.Notification{Kind: kindBudget, Title: "cap", SessionID: "s2"})
	first := deps.notify.List()[0]

	run := func(peerID string, lines ...string) {
		serveConnection(context.Background(), protocol.NewIPCStream(strings.NewReader(strings.Join(lines, "\n")+"\n"), &bytes.Buffer{}), deps, "unix", peerID, nil)
	}
	run(phone.ID, envelopeJSON(t, protocol.EventMarkNotificationsRead, protocol.MarkNotificationsReadData{IDs: []string{first.ID}}))
	if l := deps.notify.List(); !l[0].Read || l[1].Read {
		t.Fatalf("after mark: %+v", l)
	}
	if !strings.Contains(laptopOut.String(), `"type":"notifications_read"`) || !strings.Contains(laptopOut.String(), first.ID) {
		t.Fatalf("the laptop was not told:\n%s", laptopOut.String())
	}

	// Focusing on s2 on the phone reads it, and that too reaches the laptop.
	laptopOut.Reset()
	run(phone.ID, envelopeJSON(t, protocol.EventSetPresence, protocol.SetPresenceData{SessionIDs: []string{"s2"}, Focused: true, Visible: true}))
	if !deps.notify.List()[1].Read || !strings.Contains(laptopOut.String(), "notifications_read") {
		t.Fatalf("presence did not read s2: %+v\n%s", deps.notify.List(), laptopOut.String())
	}
	if !deps.peers.Viewing("s2") {
		t.Fatal("presence not stored")
	}
	_ = phoneOut
}

func TestServeConnection_SetNotifyPrefs(t *testing.T) {
	deps := newTestDeps(t)
	_, out := fakeIPCPeer(t, deps.peers, "laptop", "desktop")
	in := envelopeJSON(t, protocol.EventSetNotifyPrefs, protocol.NotifyPrefsData{Urgent: true, Quiet: true, MinTurnSec: -5, Muted: []string{kindTurnDone, kindTurnDone, "nonsense"}})
	serveConnection(context.Background(), protocol.NewIPCStream(strings.NewReader(in+"\n"), &bytes.Buffer{}), deps, "unix", "", nil)

	got := deps.settings.GetNotifyPrefs()
	if got.Normal || !got.Quiet || got.MinTurnSec != 0 || len(got.Muted) != 1 || got.Muted[0] != kindTurnDone {
		t.Fatalf("stored: %+v", got)
	}
	if !strings.Contains(out.String(), `"type":"notify_prefs"`) {
		t.Fatalf("not broadcast:\n%s", out.String())
	}
	// It survives a restart.
	reopened, err := NewServerSettings(filepath.Dir(deps.settings.filePath))
	if err != nil {
		t.Fatal(err)
	}
	if p := reopened.GetNotifyPrefs(); !p.Quiet || p.Normal {
		t.Fatalf("reloaded: %+v", p)
	}
}

// The whole path, no fakes between the agent and the feed: a turn blocks on a
// permission ask, the ask becomes an urgent notification on every peer, and
// answering it (from any device) retracts it. Then the turn's own "response
// ready" is dropped, the turn having been short.
// lockedBuf is a bytes.Buffer safe to read while a turn's goroutine writes.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestNotifications_AskRaisedThenRetractedWhenAnswered(t *testing.T) {
	deps := newTestDeps(t)
	deps.notify = NewNotificationFeed(deps.peers)
	deps.notify.prefs = func() protocol.NotifyPrefsData { p := testPrefs(); p.MinTurnSec = 30; return p }
	deps.projects.SetNotifications(deps.notify)
	deps.projects.SetStatusHub(newStatusHub(deps.peers, t.TempDir()))
	peerOut := &lockedBuf{}
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), peerOut), &fakeCloser{}, "unix", peerIdentity{Name: "phone", DeviceType: "web"})

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			for _, ev := range []string{
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"bash\",\"input\":{}}}",
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}",
				"event: content_block_stop\ndata: {\"index\":0}",
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}",
			} {
				_, _ = w.Write([]byte(ev + "\n\n"))
			}
			return
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	feed := func(lines ...string) {
		serveConnection(context.Background(), protocol.NewIPCStream(strings.NewReader(strings.Join(lines, "\n")+"\n"), io.Discard), deps, "unix", "", nil)
	}
	off := false
	feed(
		envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{Name: "p", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic", APIKey: "k", BaseURL: srv.URL}),
		envelopeJSONWithSession(t, protocol.EventSetProvider, "chat1", protocol.SetProviderData{ProviderName: "p", Model: "claude-sonnet-5"}),
		envelopeJSONWithSession(t, protocol.EventSetMode, "chat1", protocol.SetModeData{SessionID: "chat1", Mode: "read_write"}),
		envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{Prompt: "run it", SessionID: "chat1", AutoGenerateTitle: &off}),
	)

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 500; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; feed: %+v", what, deps.notify.List())
	}
	waitFor("the permission notification", func() bool { return kinds(deps.notify) == kindPermission })
	got := deps.notify.List()[0]
	if got.Priority != prioUrgent || got.SessionID != "chat1" || got.ToolCallID != "tu1" || !strings.Contains(got.Title, "bash") {
		t.Fatalf("raised: %+v", got)
	}
	if !strings.Contains(peerOut.String(), `"type":"notification"`) {
		t.Fatal("the phone was not told")
	}

	answer, _ := json.Marshal(protocol.Envelope{Type: protocol.EventPermissionResponse, SessionID: "chat1", ToolCallID: "tu1", Data: json.RawMessage(`{"allowed":true}`)})
	feed(string(answer))
	waitFor("the retraction", func() bool { return kinds(deps.notify) == "" })
	deps.turnWG.Wait()
	if l := deps.notify.List(); len(l) != 0 {
		t.Fatalf("a quick turn should end with nothing to say: %+v", l)
	}
}
