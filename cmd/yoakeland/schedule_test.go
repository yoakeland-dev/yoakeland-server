package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"strings"
	"yoakeland/internal/agent"
	"yoakeland/internal/artifact"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sched"
	"yoakeland/internal/tool"
)

type sayOK struct{}

func (sayOK) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	out := make(chan agent.ModelStreamChunk, 1)
	errs := make(chan error)
	out <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "ok"}
	close(out)
	close(errs)
	return out, errs
}

// A scheduled turn runs with no client connected, and its user message carries its origin into
// the stored history.
func TestScheduledTurnRunsWithOrigin(t *testing.T) {
	projects, err := NewProjectRegistry(sayOK{}, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pc, err := projects.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	deps := connDeps{projects: projects, peers: NewPeerRegistry(), sched: sched.New(t.TempDir()), turnWG: &sync.WaitGroup{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(ctx, deps)

	ipc := protocol.NewIPCStream(nil, io.Discard)
	raw, _ := json.Marshal(protocol.ScheduleTurnData{
		SessionID: "s1", ProjectPath: root, Text: "pick done",
		Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow},
		Origin:  &protocol.Origin{Kind: protocol.OriginArtifact, ArtifactID: "a1"},
	})
	if !handleScheduleEnvelope(ipc, deps, &protocol.Envelope{Type: protocol.EventScheduleTurn, Data: raw}) {
		t.Fatal("schedule_turn was not handled")
	}
	deps.sched.Tick()
	deps.turnWG.Wait()

	state, err := pc.SessStore.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	var user *agent.Message
	for i := range state.History {
		if state.History[i].Role == "user" {
			user = &state.History[i]
		}
	}
	if user == nil || user.Content != "pick done" || user.Origin == nil || user.Origin.Kind != protocol.OriginArtifact || user.Origin.ArtifactID != "a1" || user.Origin.ScheduleID == "" {
		t.Fatalf("user message = %+v", user)
	}
	if got := deps.sched.List("s1").Schedules; len(got) != 1 || got[0].Status != protocol.ScheduleDone || got[0].Runs != 1 {
		t.Fatalf("schedule = %+v", got)
	}
}

func TestScheduleEventsAreOwnerOnly(t *testing.T) {
	for _, e := range []protocol.EventType{protocol.EventScheduleTurn, protocol.EventListScheduled, protocol.EventCancelScheduled, protocol.EventPauseScheduled} {
		if remoteAllowed[e] {
			t.Errorf("%s is open to a restricted device", e)
		}
	}
}

func TestLimitsSpent(t *testing.T) {
	now := time.Now()
	spent := func(pct float64, reset time.Time) bool {
		return limitsSpent(protocol.ProviderRateLimits{FiveHour: &protocol.RateLimitWindow{UsedPct: pct, ResetsAt: reset.Format(time.RFC3339)}}, now)
	}
	if !spent(100, now.Add(time.Hour)) {
		t.Error("a full window that hasn't reset should hold turns back")
	}
	if spent(100, now.Add(-time.Minute)) {
		t.Error("a window past its reset time should not")
	}
	if spent(40, now.Add(time.Hour)) {
		t.Error("a window with room left should not")
	}
}

func TestArtifactSubmitSchedulesATurnAndLogsAreKept(t *testing.T) {
	deps := connDeps{peers: NewPeerRegistry(), sched: sched.New(t.TempDir())}
	ipc := protocol.NewIPCStream(nil, io.Discard)
	send := func(d protocol.ArtifactEventData) {
		raw, _ := json.Marshal(d)
		if !handleArtifactEnvelope(ipc, deps, "p1", &protocol.Envelope{Type: protocol.EventArtifactEvent, Data: raw}) {
			t.Fatal("not handled")
		}
	}
	send(protocol.ArtifactEventData{SessionID: "s9", ArtifactID: "m1#0", Kind: protocol.ArtifactSubmit, Data: map[string]string{"style": "B"}})
	got := deps.sched.List("s9").Schedules
	if len(got) != 1 || got[0].Origin.Kind != protocol.OriginArtifact || got[0].Origin.ArtifactID != "m1#0" || !strings.Contains(got[0].Text, `"style":"B"`) {
		t.Fatalf("schedule = %+v", got)
	}
	// A page answers once: a second submit is dropped without starting a turn.
	send(protocol.ArtifactEventData{SessionID: "s9", ArtifactID: "m1#0", Kind: protocol.ArtifactSubmit, Data: map[string]string{"style": "C"}})
	if n := len(deps.sched.List("s9").Schedules); n != 1 {
		t.Fatalf("%d schedules after a second submit", n)
	}
	send(protocol.ArtifactEventData{SessionID: "s9", ArtifactID: "m1#0", Kind: protocol.ArtifactError, Data: "boom at line 3"})
	if out := artifact.Default.Read("s9", ""); !strings.Contains(out, "boom at line 3") {
		t.Fatalf("logs = %q", out)
	}
	// Oversize and unknown kinds are refused; they never schedule anything.
	send(protocol.ArtifactEventData{SessionID: "s9", ArtifactID: "m1#0", Kind: protocol.ArtifactSubmit, Data: strings.Repeat("x", protocol.MaxArtifactData+1)})
	send(protocol.ArtifactEventData{SessionID: "s9", ArtifactID: "m1#0", Kind: "weird"})
	if n := len(deps.sched.List("s9").Schedules); n != 1 {
		t.Fatalf("%d schedules after bad events", n)
	}
}

func TestArtifactEventRateAndAccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ok := 0
	for i := 0; i < artifactEventsPerSec+5; i++ {
		if artifactAllow("rate-peer", now) {
			ok++
		}
	}
	if ok != artifactEventsPerSec || !artifactAllow("rate-peer", now.Add(time.Second)) {
		t.Fatalf("allowed %d in one second", ok)
	}
	if !remoteAllowed[protocol.EventArtifactEvent] {
		t.Error("a paired phone must be able to submit an artifact")
	}
}
