package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sched"
	"yoakeland/internal/tool"
)

// writesAfterAsking calls file_write once (which asks the person first), then answers.
type writesAfterAsking struct {
	mu     sync.Mutex
	rounds int
}

func (m *writesAfterAsking) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	m.mu.Lock()
	m.rounds++
	first := m.rounds == 1
	m.mu.Unlock()
	out, errs := make(chan agent.ModelStreamChunk, 1), make(chan error)
	if first {
		out <- agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{
			ID: "w1", Name: "file_write", Args: json.RawMessage(`{"path":"note.txt","content":"top secret"}`),
		}}
	} else {
		out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "all done, secret plan"}
	}
	close(out)
	close(errs)
	return out, errs
}

// wireVisibility connects the pieces main wires: who owns a chat, who may see it, and the
// notification feed and status hub the project reports to.
func wireVisibility(deps connDeps) connDeps {
	deps.projects.sameUser = deps.settings.SameUser
	deps.peers.Sees = func(network, credential, sessionID string) bool {
		return connDeps{projects: deps.projects, settings: deps.settings}.mayViewChat(deps.settings.Access(network, credential), sessionID)
	}
	deps.notify = NewNotificationFeed(deps.peers)
	deps.projects.SetNotifications(deps.notify)
	deps.projects.SetStatusHub(newStatusHub(deps.peers, deps.projects.stateDir))
	return deps
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A chat's live events (what the person typed, the model's reply, a permission prompt, a change to
// the chat, a notification) reach the chat's owner, a Full device and the local socket, and never a
// Limited person who isn't its owner, even one that is subscribed to the chat's stream.
func TestSessionEventsReachOnlyThoseWhoMaySeeTheChat(t *testing.T) {
	rec := &writesAfterAsking{}
	projects, err := NewProjectRegistry(rec, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pc, err := projects.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := wireVisibility(connDeps{projects: projects, defaultProjectRoot: root, peers: NewPeerRegistry(), settings: cfg, sched: sched.New(t.TempDir()), turnWG: &sync.WaitGroup{}})

	var bOut, fOut, sOut, localOut syncBuf
	owner := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &bOut))
	full := addPersonOn(t, deps, TierFull, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &fOut))
	stranger := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &sOut))
	local := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &localOut), nopCloser{}, "unix", peerIdentity{Name: "pc"})

	// Everyone is subscribed to the chat's stream, as a device is that once opened it. The stranger
	// shouldn't be (the open would be refused), so the stream itself has to keep the chat from it.
	for _, p := range []*Peer{full, stranger, local} {
		pc.Registry.Get(p.ipc, "sb")
	}
	off := false
	raw, _ := json.Marshal(protocol.UserPromptData{Prompt: "the secret plan", SessionID: "sb", ProjectPath: root, AutoGenerateTitle: &off})
	handleEnvelope(context.Background(), owner.ipc, deps, "tcp", owner.ID, &protocol.Envelope{Type: protocol.EventUserPrompt, TurnID: "t1", Data: raw})
	if got := ownerOf(t, deps, "sb"); got != owner.UserID {
		t.Fatalf("the chat is owned by %q", got)
	}
	waitFor(t, "the permission prompt", func() bool { return strings.Contains(bOut.String(), `"type":"permission_request"`) })

	// The owner renames the chat, answers the prompt, and the turn finishes.
	handleEnvelope(context.Background(), owner.ipc, deps, "tcp", owner.ID, &protocol.Envelope{Type: protocol.EventSetSessionTitle, Data: json.RawMessage(`{"sessionId":"sb","projectPath":"` + root + `","title":"plans"}`)})
	handleEnvelope(context.Background(), owner.ipc, deps, "tcp", owner.ID, &protocol.Envelope{Type: protocol.EventPermissionResponse, SessionID: "sb", ToolCallID: "w1", Data: json.RawMessage(`{"allowed":true}`)})
	deps.turnWG.Wait()
	waitFor(t, "the turn to finish", func() bool { return strings.Contains(bOut.String(), `"type":"turn_completed"`) })

	// A notification that stays on the list (the asks were taken back when answered).
	deps.notify.Push("", protocol.Notification{Level: "error", Kind: kindTurnError, Title: "Turn error", Body: "boom", SessionID: "sb", ProjectPath: root})

	for _, typ := range []string{"user_message", "model_delta", "permission_request", "session_updated", "notification", "pending_asks", "turn_completed"} {
		for name, out := range map[string]*syncBuf{"the owner": &bOut, "a Full device": &fOut, "the local socket": &localOut} {
			if !strings.Contains(out.String(), `"type":"`+typ+`"`) {
				t.Errorf("%s never got %s", name, typ)
			}
		}
	}
	for _, leak := range []string{"user_message", "model_delta", "permission_request", "session_updated", "notification\"", "turn_completed", "tool_call", "secret", "sb", "w1", "plans"} {
		if strings.Contains(sOut.String(), leak) {
			t.Errorf("the stranger was sent %q:\n%s", leak, sOut.String())
		}
	}
	// pending_asks does reach everyone, but only with the asks they may see.
	for _, line := range strings.Split(sOut.String(), "\n") {
		if strings.Contains(line, `"type":"pending_asks"`) && !strings.Contains(line, `"asks":[]`) {
			t.Errorf("the stranger got an ask: %s", line)
		}
	}
	if !strings.Contains(bOut.String(), `"type":"pending_asks"`) {
		t.Error("the owner never saw the pending ask")
	}

	// A connect-time list of notifications is filtered the same way.
	if out := sendAs(t, deps, stranger, protocol.Envelope{Type: protocol.EventListNotifications}); strings.Contains(out, "sb") || !strings.Contains(out, "notification_list") {
		t.Errorf("list_notifications for the stranger: %s", out)
	}
	if out := sendAs(t, deps, owner, protocol.Envelope{Type: protocol.EventListNotifications}); !strings.Contains(out, "sb") {
		t.Errorf("list_notifications for the owner: %s", out)
	}

	// With the others_chats cap the stranger is let in.
	deps.settings.mu.Lock()
	u := deps.settings.Users[stranger.UserID]
	u.Caps = map[string]any{CapOthersChats: true}
	deps.settings.Users[stranger.UserID] = u
	deps.settings.mu.Unlock()
	sOut.Reset()
	deps.peers.BroadcastEvent("", protocol.EventSessionUpdated, "sb", protocol.SessionSummary{SessionID: "sb", Title: "x"})
	if !strings.Contains(sOut.String(), "session_updated") {
		t.Error("others_chats didn't let the chat's events through")
	}
}

// A registry nobody wired a visibility check into tells a TCP peer nothing about any chat.
func TestSessionEventsFailClosedWithoutACheck(t *testing.T) {
	peers := NewPeerRegistry()
	var tcpOut, localOut syncBuf
	peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &tcpOut), nopCloser{}, "tcp", peerIdentity{Name: "p", TokenName: "c", Tier: TierLimited})
	peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &localOut), nopCloser{}, "unix", peerIdentity{Name: "pc"})
	tcpOut.Reset()
	localOut.Reset()
	peers.BroadcastEvent("", protocol.EventSessionUpdated, "s1", protocol.SessionSummary{SessionID: "s1"})
	peers.BroadcastEvent("", protocol.EventPendingAsks, "", protocol.PendingAsksData{Asks: []protocol.PendingAsk{{SessionID: "s1", ToolCallID: "tc"}}})
	if strings.Contains(tcpOut.String(), "s1") || strings.Contains(tcpOut.String(), "tc") {
		t.Errorf("a peer was told about a chat: %s", tcpOut.String())
	}
	if !strings.Contains(localOut.String(), "session_updated") || !strings.Contains(localOut.String(), `"toolCallId":"tc"`) {
		t.Errorf("the local socket missed events: %s", localOut.String())
	}
}
