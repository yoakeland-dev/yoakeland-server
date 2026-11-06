package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sched"
	"yoakeland/internal/tool"
)

// toolRecorder answers "ok" and remembers the tool names of each request.
type toolRecorder struct {
	mu    sync.Mutex
	names [][]string
}

func (r *toolRecorder) StreamTurn(_ context.Context, _ []agent.Message, tools []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	var n []string
	for _, d := range tools {
		n = append(n, d.Name)
	}
	r.mu.Lock()
	r.names = append(r.names, n)
	r.mu.Unlock()
	out, errs := make(chan agent.ModelStreamChunk, 1), make(chan error)
	out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ok"}
	close(out)
	close(errs)
	return out, errs
}

func (r *toolRecorder) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.names) == 0 {
		return nil
	}
	return r.names[len(r.names)-1]
}

func (r *toolRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.names)
}

// policyDeps is a server with a recording model and a scheduler, and one project.
func policyDeps(t *testing.T) (connDeps, *toolRecorder, string) {
	t.Helper()
	rec := &toolRecorder{}
	projects, err := NewProjectRegistry(rec, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := projects.Get(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := connDeps{projects: projects, defaultProjectRoot: root, peers: NewPeerRegistry(), settings: cfg, sched: sched.New(t.TempDir()), turnWG: &sync.WaitGroup{}}
	return deps, rec, root
}

// noPowerTools are what a person without the command, MCP and browser caps must never be offered.
func offersPowerTools(names []string) bool {
	for _, n := range names {
		if n == "bash" || strings.HasPrefix(n, "terminal_") || n == "browser" || strings.HasPrefix(n, "mcp__") {
			return true
		}
	}
	return false
}

// limitedNoCommands is a Limited person who may not run commands (so the test doesn't depend on
// whether this machine has a sandbox), with their device.
func limitedNoCommands(t *testing.T, deps connDeps, out io.Writer) *Peer {
	t.Helper()
	p := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), out))
	u := deps.settings.Users[p.UserID]
	u.Caps = map[string]any{CapCommandsSandboxed: false}
	deps.settings.Users[p.UserID] = u
	return p
}

// user_prompt, edit_message and retry_message each start a turn that carries the sender's policy.
func TestEveryTurnStartPathCarriesThePolicy(t *testing.T) {
	deps, rec, _ := policyDeps(t)
	var out syncBuf
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
	p := limitedNoCommands(t, deps, &out)
	send := func(typ string, data any) {
		raw, _ := json.Marshal(data)
		before := rec.calls()
		handleEnvelope(context.Background(), ipc, deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventType(typ), SessionID: "s1", TurnID: "t-" + typ, Data: raw})
		deps.turnWG.Wait()
		if rec.calls() == before {
			t.Fatalf("%s didn't reach the model: %s", typ, out.String())
		}
		if offersPowerTools(rec.last()) {
			t.Errorf("%s: a Limited sender's turn was offered %v", typ, rec.last())
		}
	}
	send("user_prompt", protocol.UserPromptData{Prompt: "hi", SessionID: "s1"})
	send("edit_message", protocol.EditMessageData{SessionID: "s1", MessageID: "m1", Content: "hello"})
	send("retry_message", protocol.RetryMessageData{SessionID: "s1", MessageID: "m4"})

	// The same envelopes from the local socket keep every tool.
	local := protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
	raw, _ := json.Marshal(protocol.UserPromptData{Prompt: "hi", SessionID: "s2"})
	handleEnvelope(context.Background(), local, deps, "unix", "", &protocol.Envelope{Type: protocol.EventUserPrompt, SessionID: "s2", TurnID: "t-local", Data: raw})
	deps.turnWG.Wait()
	if !slices.Contains(rec.last(), "bash") || !slices.Contains(rec.last(), "browser") {
		t.Errorf("the owner's turn lost tools: %v", rec.last())
	}
}

// A scheduled turn stores who armed it and later runs as them, under the policy they have then.
func TestScheduledTurnRunsAsItsCreator(t *testing.T) {
	deps, rec, root := policyDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(ctx, deps)
	var out syncBuf
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
	tr := addPersonOn(t, deps, TierTrusted, "", ipc)
	u := deps.settings.Users[tr.UserID]
	u.Name, u.Caps = "Tess", map[string]any{CapCommandsSandboxed: false}
	deps.settings.Users[tr.UserID] = u

	arm := func(from *Peer, network, session string) {
		raw, _ := json.Marshal(protocol.ScheduleTurnData{SessionID: session, ProjectPath: root, Text: "later", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}})
		handleEnvelope(context.Background(), ipc, deps, network, from.ID, &protocol.Envelope{Type: protocol.EventScheduleTurn, Data: raw})
	}
	arm(tr, "tcp", "s1")
	got := deps.sched.List("s1").Schedules
	if len(got) != 1 || got[0].CreatorUserID != tr.UserID || got[0].CreatorCredential != tr.TokenName {
		t.Fatalf("stored entry = %+v", got)
	}

	// Armed while they could run commands; by the time it is due they can't. It runs with what they have now.
	deps.sched.Tick()
	deps.turnWG.Wait()
	if rec.calls() != 1 || offersPowerTools(rec.last()) {
		t.Fatalf("scheduled turn tools = %v (calls %d)", rec.last(), rec.calls())
	}
	pc, _ := deps.projects.Get(root)
	st, err := pc.SessStore.Load("s1")
	if err != nil || len(st.History) == 0 || st.History[0].AuthorName != "Tess" {
		t.Fatalf("author of the scheduled message: %+v (%v)", st.History, err)
	}

	// An entry with no creator (from before users) runs as the owner, with every tool.
	_, _ = deps.sched.Add(protocol.ScheduleTurnData{SessionID: "s2", ProjectPath: root, Text: "old", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}})
	deps.sched.Tick()
	deps.turnWG.Wait()
	if !slices.Contains(rec.last(), "bash") {
		t.Errorf("a legacy schedule lost the owner's tools: %v", rec.last())
	}

	// Its creator's credential is gone: it is cancelled, not run.
	arm(tr, "tcp", "s3")
	delete(deps.settings.Tokens, tr.TokenName)
	calls := rec.calls()
	deps.sched.Tick()
	deps.turnWG.Wait()
	l := deps.sched.List("s3").Schedules
	if rec.calls() != calls || len(l) != 1 || l[0].Status != protocol.ScheduleCancelled {
		t.Fatalf("schedule of a revoked credential = %+v (calls %d -> %d)", l, calls, rec.calls())
	}
}

// Trusted lists and changes only its own schedules; the owner sees all.
func TestScheduleVisibilityAndOwnership(t *testing.T) {
	deps, _, root := policyDeps(t)
	var aOut, bOut syncBuf
	a := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &aOut))
	b := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &bOut))
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
	raw, _ := json.Marshal(protocol.ScheduleTurnData{SessionID: "s1", ProjectPath: root, Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerAt, AtMs: time.Now().Add(time.Hour).UnixMilli()}})
	handleEnvelope(context.Background(), ipc, deps, "tcp", a.ID, &protocol.Envelope{Type: protocol.EventScheduleTurn, Data: raw})
	id := deps.sched.List("").Schedules[0].ID
	list := func(from *Peer, buf *syncBuf) string {
		buf.Reset()
		handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), buf), deps, "tcp", from.ID, &protocol.Envelope{Type: protocol.EventListScheduled, Data: json.RawMessage(`{}`)})
		return buf.String()
	}
	if !strings.Contains(list(a, &aOut), id) || strings.Contains(list(b, &bOut), id) {
		t.Fatal("a Trusted person should see their own schedules and not another's")
	}
	var errOut syncBuf
	handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &errOut), deps, "tcp", b.ID, &protocol.Envelope{Type: protocol.EventCancelScheduled, Data: json.RawMessage(`{"id":"` + id + `"}`)})
	if !strings.Contains(errOut.String(), "isn't theirs") || deps.sched.List("").Schedules[0].Status == protocol.ScheduleCancelled {
		t.Fatalf("cancelled someone else's schedule: %s", errOut.String())
	}
	errOut.Reset()
	handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &errOut), deps, "tcp", b.ID, &protocol.Envelope{Type: protocol.EventPauseScheduled, Data: json.RawMessage(`{"all":true,"paused":true}`)})
	if !strings.Contains(errOut.String(), "isn't theirs") {
		t.Fatalf("a Trusted person paused the whole schedule: %s", errOut.String())
	}
}

// Lowering or revoking access cancels that credential's running turns, and only those.
func TestLoweringAccessCancelsRunningTurns(t *testing.T) {
	block := &blockingModel{started: make(chan struct{}, 4)}
	projects, err := NewProjectRegistry(block, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := projects.Get(root); err != nil {
		t.Fatal(err)
	}
	cfg, _ := NewServerSettings(t.TempDir())
	deps := connDeps{projects: projects, defaultProjectRoot: root, peers: NewPeerRegistry(), settings: cfg, turnWG: &sync.WaitGroup{}}
	var out syncBuf
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
	victim := addPersonOn(t, deps, TierTrusted, "", ipc)
	bystander := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard))
	start := func(from *Peer, sess string) {
		raw, _ := json.Marshal(protocol.UserPromptData{Prompt: "go", SessionID: sess})
		handleEnvelope(context.Background(), ipc, deps, "tcp", from.ID, &protocol.Envelope{Type: protocol.EventUserPrompt, SessionID: sess, TurnID: "t-" + sess, Data: raw})
		<-block.started
	}
	start(victim, "sv")
	start(bystander, "sb")
	if a, _ := projects.Stats(); a != 2 {
		t.Fatalf("active turns = %d, want 2", a)
	}

	before := deps.accessSnapshot()
	u := deps.settings.Users[victim.UserID]
	u.Tier = TierLimited
	deps.settings.Users[victim.UserID] = u
	if n := deps.enforceAccessChange(before); n != 1 {
		t.Fatalf("cancelled %d turns, want only the lowered person's 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a, _ := projects.Stats(); a == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a, _ := projects.Stats(); a != 1 {
		t.Fatalf("active turns after lowering = %d, want the bystander's 1", a)
	}
	if v, _ := deps.peers.Get(victim.ID); v.Tier != TierLimited {
		t.Fatalf("the lowered person's connection still says %q", v.Tier)
	}
	if b, _ := deps.peers.Get(bystander.ID); b.Tier != TierTrusted {
		t.Fatalf("the bystander's connection says %q", b.Tier)
	}

	// Revoking the bystander's credential cancels theirs too.
	before = deps.accessSnapshot()
	delete(deps.settings.Tokens, bystander.TokenName)
	if n := deps.enforceAccessChange(before); n != 1 {
		t.Fatalf("revoking cancelled %d turns, want 1", n)
	}
	deps.turnWG.Wait()
}

// blockingModel streams nothing until its turn is cancelled.
type blockingModel struct{ started chan struct{} }

func (b *blockingModel) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk), make(chan error, 1)
	b.started <- struct{}{}
	go func() {
		<-ctx.Done()
		close(c)
		e <- ctx.Err()
		close(e)
	}()
	return c, e
}

// An artifact submit and a limit continue are armed as the person who sent them.
func TestArtifactSubmitAndLimitContinueRunAsTheirSender(t *testing.T) {
	deps, _, root := policyDeps(t)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
	p := addPersonOn(t, deps, TierLimited, "", ipc)

	raw, _ := json.Marshal(protocol.ArtifactEventData{SessionID: "s1", ProjectPath: root, ArtifactID: "a1", Kind: protocol.ArtifactSubmit, Data: "x"})
	if !handleArtifactEnvelope(ipc, deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventArtifactEvent, Data: raw}) {
		t.Fatal("artifact_event not handled")
	}
	got := deps.sched.List("s1").Schedules
	if len(got) != 1 || got[0].CreatorUserID != p.UserID || got[0].CreatorCredential != p.TokenName {
		t.Fatalf("artifact submit schedule = %+v", got)
	}

	deps.sched.SetHit(protocol.LimitHit{SessionID: "s2", ProjectPath: root, ResetsAtMs: time.Now().Add(time.Hour).UnixMilli()}, true)
	raw, _ = json.Marshal(protocol.LimitContinueData{SessionID: "s2"})
	handleLimitEnvelope(ipc, deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventLimitContinue, Data: raw})
	got = deps.sched.List("s2").Schedules
	if len(got) != 1 || got[0].CreatorUserID != p.UserID {
		t.Fatalf("limit continue schedule = %+v", got)
	}

	// A continue the server arms by itself after a failed turn runs as that turn's starter.
	deps.sched.SetHit(protocol.LimitHit{SessionID: "s3", ProjectPath: root, ResetsAtMs: time.Now().Add(time.Hour).UnixMilli(), UserID: p.UserID, Credential: p.TokenName}, true)
	hit, _ := deps.sched.Hit("s3")
	if err := armContinue(deps, hit, agent.TurnActor{UserID: hit.UserID, Credential: hit.Credential}); err != nil {
		t.Fatal(err)
	}
	if got = deps.sched.List("s3").Schedules; len(got) != 1 || got[0].CreatorCredential != p.TokenName {
		t.Fatalf("automatic continue schedule = %+v", got)
	}
}
