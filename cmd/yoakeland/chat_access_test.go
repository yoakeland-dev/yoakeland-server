package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/projectstore"
	"yoakeland/internal/protocol"
)

// addDeviceOf gives an existing person another device, held to maxTier, on a stream the test reads.
func addDeviceOf(t *testing.T, deps connDeps, userID, maxTier string, out *syncBuf) *Peer {
	t.Helper()
	s := deps.settings
	s.mu.Lock()
	u := s.Users[userID]
	cred := "dev-" + randomID()
	s.Tokens[cred] = TokenRecord{Token: "tok-" + cred, UserID: userID, MaxTier: maxTier}
	s.mu.Unlock()
	tier := minTier(u.Tier, maxTier)
	return deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), out), nopCloser{}, "tcp",
		peerIdentity{Name: cred, TokenName: cred, Restricted: tier != TierFull, Tier: tier, UserID: userID})
}

// sendAs runs env from a TCP peer (nil: the local socket) and returns what the connection wrote back.
func sendAs(t *testing.T, deps connDeps, from *Peer, env protocol.Envelope) string {
	t.Helper()
	var out syncBuf
	network, id := "unix", ""
	if from != nil {
		network, id = "tcp", from.ID
	}
	handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &out), deps, network, id, &env)
	deps.turnWG.Wait()
	return out.String()
}

func envOf(typ protocol.EventType, sessionID string, data any) protocol.Envelope {
	raw, _ := json.Marshal(data)
	return protocol.Envelope{Type: typ, SessionID: sessionID, TurnID: "t-" + string(typ), Data: raw}
}

func notTheirChat(out string) bool { return strings.Contains(out, "isn't their chat") }

func projectStore(t *testing.T, deps connDeps) agent.SessionStore {
	t.Helper()
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	return pc.SessStore
}

func ownerOf(t *testing.T, deps connDeps, id string) string {
	t.Helper()
	st, err := projectStore(t, deps).Load(id)
	if err != nil {
		t.Fatalf("loading %s: %v", id, err)
	}
	return st.OwnerID
}

// A session records who created it: the sender's person, and the owner for the local socket.
func TestChatOwnerIsSetFromTheSender(t *testing.T) {
	deps, _, _ := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	if out := sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "hi", SessionID: "s-a"})); refused(out) {
		t.Fatalf("a Limited person can't start a chat of their own: %s", out)
	}
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "hi", SessionID: "s-o"}))
	if got := ownerOf(t, deps, "s-a"); got != a.UserID {
		t.Errorf("s-a is owned by %q, want %q", got, a.UserID)
	}
	if got := ownerOf(t, deps, "s-o"); got != ownerID {
		t.Errorf("a chat from the local socket is owned by %q, want the owner", got)
	}
}

// Every event that acts on a chat is refused for someone who doesn't own it, whichever way the
// chat is named, and works on their own.
func TestOwnChatEventsRefuseSomeoneElsesChat(t *testing.T) {
	deps, _, _ := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "secret", SessionID: "s-o"}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "mine", SessionID: "s-a"}))

	var own []protocol.EventType
	for ev, req := range eventAccess {
		if req.ownChat {
			own = append(own, ev)
		}
	}
	if len(own) < 25 {
		t.Fatalf("only %d own-chat events in the table", len(own))
	}
	body := func(id string) map[string]any {
		return map[string]any{"sessionId": id, "messageId": "m1", "content": "x", "prompt": "x", "requestId": "r", "mode": "read_write",
			"providerName": "p", "target": filepath.Join(deps.defaultProjectRoot, "x"), "allowed": true, "answer": "x", "title": "stolen", "archived": true}
	}
	for _, ev := range own {
		for name, mk := range map[string]func(string) protocol.Envelope{
			"in the envelope": func(id string) protocol.Envelope { return envOf(ev, id, map[string]any{}) },
			"in the payload":  func(id string) protocol.Envelope { return envOf(ev, "", body(id)) },
			"in both":         func(id string) protocol.Envelope { return envOf(ev, id, body(id)) },
			"in the payload, other case": func(id string) protocol.Envelope {
				return envOf(ev, "", map[string]any{"SESSIONID": id, "ProjectPath": ""})
			},
		} {
			if out := sendAs(t, deps, a, mk("s-o")); !notTheirChat(out) {
				t.Errorf("%s (%s) on someone else's chat: %q", ev, name, out)
			}
		}
	}
	if st, _ := projectStore(t, deps).Load("s-o"); st.Title == "stolen" || st.Archived || len(st.History) != 2 {
		t.Errorf("the owner's chat changed: title %q archived %v messages %d", st.Title, st.Archived, len(st.History))
	}

	// Their own chat gets past the check.
	for _, ev := range []protocol.EventType{protocol.EventGetSessionHistory, protocol.EventSetSessionTitle, protocol.EventSetMode} {
		if out := sendAs(t, deps, a, envOf(ev, "s-a", body("s-a"))); notTheirChat(out) {
			t.Errorf("%s on their own chat was refused: %s", ev, out)
		}
	}
	// A Full device and the others_chats cap see every chat.
	full := addPerson(t, deps, TierFull)
	if out := sendAs(t, deps, full, envOf(protocol.EventGetSessionHistory, "s-o", body("s-o"))); notTheirChat(out) || refused(out) {
		t.Errorf("Full refused: %s", out)
	}
	nosy := addPerson(t, deps, TierLimited)
	deps.settings.mu.Lock()
	u := deps.settings.Users[nosy.UserID]
	u.Caps = map[string]any{CapOthersChats: true}
	deps.settings.Users[nosy.UserID] = u
	deps.settings.mu.Unlock()
	if out := sendAs(t, deps, nosy, envOf(protocol.EventGetSessionHistory, "s-o", body("s-o"))); notTheirChat(out) {
		t.Errorf("others_chats refused: %s", out)
	}
}

// The chat the check looks at is the chat the handler acts on: a peer can't pass on its own chat and
// act on another through the other field, or through the default chat an empty id means.
func TestMismatchedSessionIDsCantSlipPastTheCheck(t *testing.T) {
	deps, _, _ := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "secret", SessionID: "s-o"}))
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "default", protocol.UserPromptData{Prompt: "secret", SessionID: "default"}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "mine", SessionID: "s-a"}))

	for _, tc := range []struct {
		name string
		env  protocol.Envelope
	}{
		// set_mode and get_session_history act on the payload's id; others on the envelope's.
		{"set_mode: mine in the envelope, theirs in the payload", envOf(protocol.EventSetMode, "s-a", protocol.SetModeData{SessionID: "s-o", Mode: "read_only"})},
		{"set_mode: theirs in the envelope, mine in the payload", envOf(protocol.EventSetMode, "s-o", protocol.SetModeData{SessionID: "s-a", Mode: "read_only"})},
		{"get_session_history: mine in the envelope", envOf(protocol.EventGetSessionHistory, "s-a", protocol.GetSessionHistoryData{SessionID: "s-o"})},
		{"set_model_options", envOf(protocol.EventSetModelOptions, "s-a", protocol.SetModelOptionsData{SessionID: "s-o"})},
		{"approve_plan", envOf(protocol.EventApprovePlan, "s-a", protocol.ApprovePlanData{SessionID: "s-o", Mode: "read_write"})},
		{"steer_turn", envOf(protocol.EventSteerTurn, "s-a", protocol.SteerTurnData{SessionID: "s-o", Prompt: "x"})},
		{"cancel_turn on the envelope's id, theirs in the payload", envOf(protocol.EventCancelTurn, "s-a", map[string]any{"sessionId": "s-o", "turnId": "t"})},
		{"permission_response with their chat's tool call", func() protocol.Envelope {
			e := envOf(protocol.EventPermissionResponse, "s-a", map[string]any{"sessionId": "s-o", "allowed": true})
			e.ToolCallID = "tc-1"
			return e
		}()},
		{"artifact_event: the payload's id wins in the handler", envOf(protocol.EventArtifactEvent, "s-a", protocol.ArtifactEventData{SessionID: "s-o", ArtifactID: "m1#0", Kind: protocol.ArtifactError, Data: "x"})},
		{"set_session_layout linking to their chat", envOf(protocol.EventSetSessionLayout, "s-a", map[string]any{"sessionId": "s-a", "linkedSessionId": "s-o"})},
		// An empty id is the "default" chat in every handler that falls back to it.
		{"set_mode with no id", envOf(protocol.EventSetMode, "", protocol.SetModeData{Mode: "read_only"})},
		{"set_provider with no id", envOf(protocol.EventSetProvider, "", protocol.SetProviderData{ProviderName: "p"})},
		{"set_model_options with no id", envOf(protocol.EventSetModelOptions, "", protocol.SetModelOptionsData{})},
		{"cancel_turn with no id", envOf(protocol.EventCancelTurn, "", protocol.CancelTurnData{TurnID: "t"})},
		{"permission_response with no id", envOf(protocol.EventPermissionResponse, "", protocol.PermissionResponseData{Allowed: true})},
		{"ask_user_answer with no id", envOf(protocol.EventAskUserAnswer, "", protocol.AskUserAnswerData{Answer: "x"})},
		{"user_prompt with no id", envOf(protocol.EventUserPrompt, "", protocol.UserPromptData{Prompt: "x"})},
		{"user_prompt naming default", envOf(protocol.EventUserPrompt, "default", protocol.UserPromptData{Prompt: "x", SessionID: "default"})},
	} {
		if out := sendAs(t, deps, a, tc.env); !notTheirChat(out) {
			t.Errorf("%s: %q", tc.name, out)
		}
	}
	if st, _ := projectStore(t, deps).Load("default"); len(st.History) != 2 {
		t.Errorf("the owner's default chat got %d messages", len(st.History))
	}
	if st, _ := projectStore(t, deps).Load("s-o"); st.Mode != "" && st.Mode != string(agent.ModeReadWrite) {
		t.Errorf("mode of the owner's chat = %q", st.Mode)
	}
}

// A chat with no owner belongs to the owner, a merged person keeps their chats, and anything that
// can't be read is refused.
func TestChatOwnershipEdgeCases(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	store := projectStore(t, deps)
	ask := func(p *Peer, id string) string {
		return sendAs(t, deps, p, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: id, ProjectPath: root}))
	}

	// from before owners: no ownerId on the file.
	if err := store.Save(&agent.SessionState{SessionID: "old", History: []agent.Message{{ID: "m1", Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if out := ask(a, "old"); !notTheirChat(out) {
		t.Errorf("an old chat is somebody's to read: %s", out)
	}
	if out := ask(nil, "old"); refused(out) {
		t.Errorf("the owner can't read an old chat: %s", out)
	}

	// a chat that doesn't exist yet is the sender's to create.
	if out := ask(a, "fresh"); notTheirChat(out) {
		t.Errorf("a new chat was refused: %s", out)
	}

	// a file that can't be read is refused rather than guessed at.
	dir := projectstore.DataDir(deps.projects.stateDir, deps.defaultProjectRoot)
	if err := os.WriteFile(filepath.Join(dir, "sessions", "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := ask(a, "broken"); !notTheirChat(out) {
		t.Errorf("an unreadable chat was let through: %s", out)
	}

	// a project the server doesn't know has no answer, so no access.
	if out := sendAs(t, deps, a, envOf(protocol.EventGetSessionHistory, "", protocol.GetSessionHistoryData{SessionID: "x1", ProjectPath: t.TempDir()})); !refused(out) {
		t.Errorf("a chat in an unknown project: %s", out)
	}

	// merge_users: the chats of the merged person belong to the person they were merged into.
	b := addPerson(t, deps, TierLimited)
	sendAs(t, deps, b, envOf(protocol.EventUserPrompt, "s-b", protocol.UserPromptData{Prompt: "hi", SessionID: "s-b", ProjectPath: root}))
	c := addPerson(t, deps, TierLimited)
	if out := ask(c, "s-b"); !notTheirChat(out) {
		t.Fatalf("a stranger read it: %s", out)
	}
	if err := deps.settings.MergeUsers(b.UserID, c.UserID); err != nil {
		t.Fatal(err)
	}
	deps.projects.mu.Lock()
	delete(deps.projects.owners, "s-b") // the cache holds the id as stored; the check must resolve it either way
	deps.projects.mu.Unlock()
	if out := ask(c, "s-b"); notTheirChat(out) {
		t.Errorf("a merged person lost their chat: %s", out)
	}
	cNow := deps.accessFor("tcp", c.ID)
	if !deps.mayAct(cNow, b.UserID) {
		t.Error("an alias of the merged person doesn't resolve")
	}
}

// A Limited phone and a Full desktop of one person both drive their chat; another person can't
// approve its tools, steer it or cancel it.
func TestOwnTurnsAreControlledByThePerson(t *testing.T) {
	deps, _, root := policyDeps(t)
	var phoneOut, pcOut, otherOut syncBuf
	alice := addPerson(t, deps, TierFull)
	phone2 := addDeviceOf(t, deps, alice.UserID, TierLimited, &phoneOut)
	desktop := addDeviceOf(t, deps, alice.UserID, "", &pcOut)
	if deps.accessFor("tcp", phone2.ID).Full() || !deps.accessFor("tcp", desktop.ID).Full() {
		t.Fatal("the phone should be Limited and the desktop Full")
	}
	other := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &otherOut))

	sendAs(t, deps, phone2, envOf(protocol.EventUserPrompt, "s1", protocol.UserPromptData{Prompt: "go", SessionID: "s1", ProjectPath: root}))
	if got := ownerOf(t, deps, "s1"); got != alice.UserID {
		t.Fatalf("the phone's chat is owned by %q, want %q", got, alice.UserID)
	}
	controls := []protocol.Envelope{
		func() protocol.Envelope {
			e := envOf(protocol.EventPermissionResponse, "s1", protocol.PermissionResponseData{Allowed: true})
			e.ToolCallID = "tc"
			return e
		}(),
		envOf(protocol.EventSteerTurn, "s1", protocol.SteerTurnData{SessionID: "s1", Prompt: "more", ProjectPath: root}),
		envOf(protocol.EventCancelTurn, "s1", protocol.CancelTurnData{TurnID: "t1"}),
		envOf(protocol.EventCancelSubagent, "s1", protocol.CancelSubagentData{ToolCallID: "tc"}),
	}
	for _, e := range controls {
		if out := sendAs(t, deps, other, e); !notTheirChat(out) {
			t.Errorf("%s: another person was allowed: %q", e.Type, out)
		}
		for name, p := range map[string]*Peer{"the Limited phone": phone2, "the person's Full desktop": desktop} {
			if out := sendAs(t, deps, p, e); notTheirChat(out) {
				t.Errorf("%s: %s was refused: %q", e.Type, name, out)
			}
		}
	}
}

func hasSession(out, id string) bool { return strings.Contains(out, `"sessionId":"`+id+`"`) }

// list_sessions holds only the sender's own chats, in a project and among the project-less chats.
func TestListSessionsIsFilteredToOwnChats(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "hi", SessionID: "s-o", ProjectPath: root}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "hi", SessionID: "s-a", ProjectPath: root}))
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "c-o", protocol.UserPromptData{Prompt: "hi", SessionID: "c-o", ProjectPath: protocol.ChatProject}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "c-a", protocol.UserPromptData{Prompt: "hi", SessionID: "c-a", ProjectPath: protocol.ChatProject}))

	for _, project := range []string{root, protocol.ChatProject} {
		mine, theirs := "s-a", "s-o"
		if project == protocol.ChatProject {
			mine, theirs = "c-a", "c-o"
		}
		got := sendAs(t, deps, a, envOf(protocol.EventListSessions, "", protocol.ListSessionsData{ProjectPath: project}))
		if !hasSession(got, mine) || hasSession(got, theirs) {
			t.Errorf("%s: a Limited person's list = %s", project, got)
		}
		all := sendAs(t, deps, nil, envOf(protocol.EventListSessions, "", protocol.ListSessionsData{ProjectPath: project}))
		if !hasSession(all, mine) || !hasSession(all, theirs) {
			t.Errorf("%s: the owner's list = %s", project, all)
		}
	}
}

// get_usage counts a Limited person's own chats only, never the deleted chats' totals or other
// tools' transcripts; Trusted and Full count everyone's.
func TestUsageIsLimitedToOwnChats(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	trusted := addPerson(t, deps, TierTrusted)
	store := projectStore(t, deps)
	spend := func(id, owner string, usd float64) {
		msg := agent.Message{ID: "m2", Role: "assistant", Provider: "p", Model: "m", CreatedAt: time.Now(), Usage: &protocol.UsageData{InputTokens: 10, OutputTokens: 5, CostUSD: usd}}
		if err := store.Save(&agent.SessionState{SessionID: id, OwnerID: owner, CreatedAt: time.Now(), History: []agent.Message{msg}}); err != nil {
			t.Fatal(err)
		}
	}
	spend("u-a", a.UserID, 1)
	spend("u-o", "", 5) // no owner on the file: the owner's
	_ = root

	total := func(p *Peer) float64 {
		var out syncBuf
		network, id := "unix", ""
		if p != nil {
			network, id = "tcp", p.ID
		}
		usageReports.Lock()
		usageReports.key = "" // not the cached answer of an earlier asker
		usageReports.Unlock()
		handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &out), deps, network, id,
			&protocol.Envelope{Type: protocol.EventGetUsage, Data: json.RawMessage(`{"external":true}`)})
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), "usage_report") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])), &env); err != nil {
			t.Fatalf("no usage report: %q", out.String())
		}
		var rep protocol.UsageReportData
		_ = json.Unmarshal(env.Data, &rep)
		sum := 0.0
		for _, r := range rep.Rows {
			sum += r.CostUSD
		}
		return sum
	}
	if got := total(a); got != 1 {
		t.Errorf("a Limited person's spend = %v, want 1", got)
	}
	if got := total(trusted); got != 6 {
		t.Errorf("a Trusted person's spend = %v, want 6", got)
	}
	if got := total(nil); got != 6 {
		t.Errorf("the owner's spend = %v, want 6", got)
	}
}

// An attachment is read only through its own chat; project media by path stays open to anyone who
// can read the project.
func TestReadMediaChecksTheAttachmentsChat(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	pic := filepath.Join(root, "pic.png")
	if err := os.WriteFile(pic, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "see", SessionID: "s-o", ProjectPath: root, Attachments: []string{pic}}))

	read := func(p *Peer, typ protocol.EventType, path, session string) string {
		return sendAs(t, deps, p, envOf(typ, "", map[string]any{"requestId": "r", "projectPath": root, "path": path, "sessionId": session}))
	}
	for _, typ := range []protocol.EventType{protocol.EventReadMedia, protocol.EventReadMediaChunk} {
		if out := read(nil, typ, pic, "s-o"); strings.Contains(out, `"error":"`) {
			t.Errorf("%s: the chat's owner was refused: %s", typ, out)
		}
		if out := read(a, typ, pic, "s-o"); !strings.Contains(out, "isn't this person's") {
			t.Errorf("%s: someone else read an attachment: %s", typ, out)
		}
		if out := read(a, typ, "pic.png", ""); strings.Contains(out, `"error":"`) {
			t.Errorf("%s: project media by path was refused: %s", typ, out)
		}
	}
}

// Usage-limit cards list only the chats the viewer may see, and never whose credential started the turn.
func TestLimitHitsInTheScheduleAreFiltered(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierTrusted)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "hi", SessionID: "s-o", ProjectPath: root}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "hi", SessionID: "s-a", ProjectPath: root}))
	deps.sched.SetHit(protocol.LimitHit{SessionID: "s-o", Credential: "owner-phone"}, true)
	deps.sched.SetHit(protocol.LimitHit{SessionID: "s-a", Credential: "a-phone"}, true)

	got := visibleSchedules(deps, deps.sched.List(""), deps.accessFor("tcp", a.ID))
	if len(got.LimitHits) != 1 || got.LimitHits[0].SessionID != "s-a" || got.LimitHits[0].Credential != "" {
		t.Errorf("a Trusted person sees %+v", got.LimitHits)
	}
	if all := visibleSchedules(deps, deps.sched.List(""), localAccess()); len(all.LimitHits) != 2 {
		t.Errorf("the owner sees %+v", all.LimitHits)
	}
	// And the schedule itself can't put a message in someone else's chat.
	out := sendAs(t, deps, a, envOf(protocol.EventScheduleTurn, "", protocol.ScheduleTurnData{SessionID: "s-o", ProjectPath: root, Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}}))
	if !notTheirChat(out) {
		t.Errorf("schedule_turn into someone else's chat: %q", out)
	}
}

// read_chat reads another chat only for the person who owns it (or with others_chats); the
// owner's own turns read anything in the project.
func TestReadChatFollowsThePolicysUser(t *testing.T) {
	deps, _, root := policyDeps(t)
	a := addPerson(t, deps, TierLimited)
	b := addPerson(t, deps, TierLimited)
	sendAs(t, deps, nil, envOf(protocol.EventUserPrompt, "s-o", protocol.UserPromptData{Prompt: "secret plan", SessionID: "s-o", ProjectPath: root}))
	sendAs(t, deps, a, envOf(protocol.EventUserPrompt, "s-a", protocol.UserPromptData{Prompt: "my notes", SessionID: "s-a", ProjectPath: root}))
	pc, _ := deps.projects.Get(root)

	read := func(ctx context.Context, id string) error {
		_, _, err := pc.chats.ReadChat(ctx, id)
		return err
	}
	policyOf := func(p *Peer) context.Context {
		return agent.WithTurnPolicy(context.Background(), turnPolicyFor(deps.accessFor("tcp", p.ID)))
	}
	if err := read(policyOf(a), "s-a"); err != nil {
		t.Errorf("a person can't read their own chat: %v", err)
	}
	if err := read(policyOf(a), "s-o"); err == nil {
		t.Error("a Limited person's agent read the owner's chat")
	}
	if err := read(policyOf(b), "s-a"); err == nil {
		t.Error("a Limited person's agent read another person's chat")
	}
	if err := read(context.Background(), "s-o"); err != nil {
		t.Errorf("the owner's turn can't read: %v", err)
	}
	if err := read(agent.WithTurnPolicy(context.Background(), &agent.TurnPolicy{UserID: b.UserID, OthersChats: true}), "s-o"); err != nil {
		t.Errorf("others_chats should read: %v", err)
	}
	if err := read(agent.WithTurnPolicy(context.Background(), &agent.TurnPolicy{}), "s-a"); err == nil {
		t.Error("a policy with no person read a chat")
	}
}
