package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/sched"
)

func TestParseLimitReset(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	now := time.Date(2026, 11, 1, 14, 0, 0, 0, loc)
	cases := []struct {
		msg  string
		want time.Time
		ok   bool
	}{
		{"Claude AI usage limit reached|1793563200", time.Unix(1793563200, 0), true},
		{"limit reached, resets 2026-11-01T18:30:00Z", time.Date(2026, 11, 1, 18, 30, 0, 0, time.UTC), true},
		{"You've hit your limit. Try again in 2 hours 5 minutes", now.Add(2*time.Hour + 5*time.Minute), true},
		{"usage limit reached; try again in 45 min", now.Add(45 * time.Minute), true},
		{"limit hit, resets 3:05pm", time.Date(2026, 11, 1, 15, 5, 0, 0, loc), true},
		{"limit hit, resets 1:00 pm", time.Date(2026, 11, 2, 13, 0, 0, 0, loc), true}, // already past today: tomorrow
		{"limit hit, resets 12am", time.Date(2026, 11, 2, 0, 0, 0, 0, loc), false},    // no minutes and am/pm is enough? see below
		{"limit reached, resets 5", time.Time{}, false},                               // a bare number is not a time
		{"something else entirely", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := parseLimitReset(c.msg, now, loc)
		if c.msg == "limit hit, resets 12am" { // "12am" has am/pm, so it is a time: midnight, next one
			if !ok || !got.Equal(c.want) {
				t.Errorf("%q = %v %v", c.msg, got, ok)
			}
			continue
		}
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("%q = %v %v, want %v %v", c.msg, got, ok, c.want, c.ok)
		}
	}
}

func TestClassifyLimit(t *testing.T) {
	now := time.Now()
	spent := protocol.ProviderRateLimits{FiveHour: &protocol.RateLimitWindow{UsedPct: 100, ResetsAt: now.Add(90 * time.Minute).Format(time.RFC3339)}}
	// The provider's own numbers win over the message.
	got, est, ok := classifyLimit(errors.New("claude: usage limit reached|"+"1"), spent, now, time.UTC)
	if !ok || est || got.Sub(now) < 89*time.Minute {
		t.Fatalf("with numbers: %v %v %v", got, est, ok)
	}
	// A strong phrase with no time anywhere is a limit of unknown length.
	if _, est, ok := classifyLimit(errors.New("claude: You've hit your weekly limit"), protocol.ProviderRateLimits{}, now, time.UTC); !ok || !est {
		t.Fatalf("strong, no time: %v %v", est, ok)
	}
	// A weak phrase alone (an API throttle) is just an error; with a reset time it is a limit.
	if _, _, ok := classifyLimit(errors.New("429 too many requests"), protocol.ProviderRateLimits{}, now, time.UTC); ok {
		t.Fatal("a plain 429 counted as a usage limit")
	}
	if _, _, ok := classifyLimit(errors.New("quota exceeded, try again in 10 minutes"), protocol.ProviderRateLimits{}, now, time.UTC); !ok {
		t.Fatal("a quota error with a reset time was missed")
	}
	// Other limits the CLI words the same way are not the subscription running out, even with spent windows.
	for _, m := range []string{
		"Claude is temporarily limiting requests to the usage page (not your usage limit). Try again in a moment.",
		"Context limit reached", "Concurrent subagent limit reached", "Budget limit reached ($5.00)", "Fast limit reached and temporarily disabled",
	} {
		if _, _, ok := classifyLimit(errors.New(m), spent, now, time.UTC); ok {
			t.Errorf("%q was taken for a usage limit", m)
		}
	}
	for _, m := range []string{"You've hit your session limit · resets 3pm", "Opus limit reached", "Claude AI usage limit reached|1793563200"} {
		if _, _, ok := classifyLimit(errors.New(m), protocol.ProviderRateLimits{}, now, time.UTC); !ok {
			t.Errorf("%q was missed", m)
		}
	}
	// Numbers that say the window already reset are stale and don't count as a time.
	stale := protocol.ProviderRateLimits{FiveHour: &protocol.RateLimitWindow{UsedPct: 100, ResetsAt: now.Add(-time.Hour).Format(time.RFC3339)}}
	if _, est, ok := classifyLimit(errors.New("usage limit reached"), stale, now, time.UTC); !ok || !est {
		t.Fatalf("stale numbers: %v %v", est, ok)
	}
	// A CLI that fails with no explanation while the plan's window is spent: that is the limit.
	if got, est, ok := classifyLimit(errors.New("claude process exited with status 1"), spent, now, time.UTC); !ok || est || got.Sub(now) < 89*time.Minute {
		t.Fatalf("silent failure with a spent window: %v %v %v", got, est, ok)
	}
	// But not for a spent per-model bucket (the chat may run another model), nor for a cancel.
	opus := protocol.ProviderRateLimits{Other: []protocol.OtherRateLimitWindow{{WindowMins: 7 * 24 * 60, Label: "Opus", RateLimitWindow: protocol.RateLimitWindow{UsedPct: 100, ResetsAt: now.Add(time.Hour).Format(time.RFC3339)}}}}
	if _, _, ok := classifyLimit(errors.New("connection reset by peer"), opus, now, time.UTC); ok {
		t.Fatal("a spent Opus bucket made an unrelated error a limit")
	}
	if _, _, ok := classifyLimit(context.Canceled, spent, now, time.UTC); ok {
		t.Fatal("a cancelled turn counted as a limit")
	}
	if limitBackoff(0) != 5*time.Minute || limitBackoff(1) != 10*time.Minute || limitBackoff(9) != time.Hour {
		t.Fatal("backoff wrong")
	}
}

func limitDeps(t *testing.T) (connDeps, *ProjectContext) {
	t.Helper()
	settings, err := NewServerSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects, err := NewProjectRegistry(sayOK{}, "p", 0, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := projects.Get(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return connDeps{projects: projects, peers: NewPeerRegistry(), sched: sched.New(t.TempDir()), settings: settings, turnWG: &sync.WaitGroup{}}, pc
}

func limitErr(after string) error {
	return errors.New("claude: usage limit reached, try again in " + after)
}

func TestLimitHitAskRecordsButDoesNotArm(t *testing.T) {
	deps, pc := limitDeps(t)
	if !onLimitHit(deps, pc, "s1", "p", limitErr("2 hours"), nil) {
		t.Fatal("not recognised")
	}
	hit, ok := deps.sched.Hit("s1")
	if !ok || hit.Estimated || time.UnixMilli(hit.ResetsAtMs).Sub(time.Now()) < 119*time.Minute {
		t.Fatalf("hit = %+v", hit)
	}
	if _, armed := deps.sched.AutoFor("s1"); armed {
		t.Fatal("ask mode armed a continue by itself")
	}
}

func TestLimitHitContinueArmsOnceAndSurvivesARestart(t *testing.T) {
	deps, pc := limitDeps(t)
	_ = deps.settings.SetLimitAction(protocol.LimitActionContinue)
	onLimitHit(deps, pc, "s1", "p", limitErr("2 hours"), nil)
	onLimitHit(deps, pc, "s1", "p", limitErr("2 hours"), nil) // a second hit for the same chat
	auto, ok := deps.sched.AutoFor("s1")
	if !ok || auto.Trigger.Kind != protocol.TriggerLimitReset || auto.Origin.Label != protocol.OriginLabelLimit || auto.NextRunAtMs < time.Now().Add(119*time.Minute).UnixMilli() {
		t.Fatalf("auto = %+v", auto)
	}
	n := 0
	for _, e := range deps.sched.List("").Schedules {
		if sched.IsAuto(e) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d continues for one chat", n)
	}
	// Server restarts: both the hit and the waiting continue are still there, and not due yet.
	again := sched.New(deps.sched.Dir())
	if _, ok := again.Hit("s1"); !ok {
		t.Fatal("hit lost over a restart")
	}
	if _, ok := again.AutoFor("s1"); !ok {
		t.Fatal("continue lost over a restart")
	}
}

func TestLimitHitEstimatesAndGivesUp(t *testing.T) {
	deps, pc := limitDeps(t)
	_ = deps.settings.SetLimitAction(protocol.LimitActionContinue)
	noTime := errors.New("claude: You've hit your weekly limit")
	onLimitHit(deps, pc, "s1", "p", noTime, nil)
	h, _ := deps.sched.Hit("s1")
	if !h.Estimated || time.UnixMilli(h.ResetsAtMs).Sub(time.Now()) > 6*time.Minute {
		t.Fatalf("first guess = %+v", h)
	}
	// The continue itself runs and hits the limit again: the guess doubles.
	auto, _ := deps.sched.AutoFor("s1")
	origin := auto.Origin
	deps.sched.Resolve("s1")                                                   // as the running continue would no longer be waiting
	deps.sched.Run = func(protocol.ScheduledTurn, func()) error { return nil } // its turn is "running" and never ends here
	autoAgain := func(attempt int) protocol.LimitHit {
		a, err := deps.sched.Add(protocol.ScheduleTurnData{SessionID: "s1", Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerLimitReset}, Origin: &protocol.Origin{Kind: protocol.OriginScheduled, Label: protocol.OriginLabelLimit}, Attempt: attempt})
		if err != nil {
			t.Fatal(err)
		}
		deps.sched.Tick() // the continue starts: no longer waiting
		o := a.Origin
		onLimitHit(deps, pc, "s1", "p", noTime, &o)
		h, _ := deps.sched.Hit("s1")
		return h
	}
	if origin.WaitedMs <= 0 {
		t.Fatalf("limit-continue origin has no wait: %+v", origin)
	}
	if h := autoAgain(0); h.Attempt != 1 || time.UnixMilli(h.ResetsAtMs).Sub(time.Now()) < 9*time.Minute {
		t.Fatalf("second = %+v", h)
	}
	deps.sched.Resolve("s1")
	if h := autoAgain(limitMaxAttempts - 1); !h.GaveUp {
		t.Fatalf("did not give up: %+v", h)
	}
	if _, armed := deps.sched.AutoFor("s1"); armed {
		t.Fatal("armed again after giving up")
	}
	// An explicit try-again starts over.
	ipc := protocol.NewIPCStream(nil, io.Discard)
	raw, _ := json.Marshal(protocol.LimitContinueData{SessionID: "s1"})
	handleLimitEnvelope(ipc, deps, &protocol.Envelope{Type: protocol.EventLimitContinue, Data: raw})
	h2, _ := deps.sched.Hit("s1")
	a, armed := deps.sched.AutoFor("s1")
	if !armed || h2.GaveUp || h2.Attempt != 0 || a.Attempt != 0 {
		t.Fatalf("after try again: %+v %+v %v", h2, a, armed)
	}
}

func TestLimitTooFarIsNotWaitedOut(t *testing.T) {
	deps, pc := limitDeps(t)
	_ = deps.settings.SetLimitAction(protocol.LimitActionContinue)
	onLimitHit(deps, pc, "s1", "p", limitErr("30 days"), nil)
	h, _ := deps.sched.Hit("s1")
	if !h.TooFar {
		t.Fatalf("hit = %+v", h)
	}
	if _, armed := deps.sched.AutoFor("s1"); armed {
		t.Fatal("armed a continue a month out")
	}
	ipc := protocol.NewIPCStream(nil, io.Discard)
	raw, _ := json.Marshal(protocol.LimitContinueData{SessionID: "s1"})
	handleLimitEnvelope(ipc, deps, &protocol.Envelope{Type: protocol.EventLimitContinue, Data: raw})
	if _, armed := deps.sched.AutoFor("s1"); armed {
		t.Fatal("limit_continue armed one a month out")
	}
}

func TestPickingUpTheChatEndsTheLimitHit(t *testing.T) {
	deps, pc := limitDeps(t)
	_ = deps.settings.SetLimitAction(protocol.LimitActionContinue)
	onLimitHit(deps, pc, "s1", "p", limitErr("1 hour"), nil)
	clearLimitOnSend(deps, "s1")
	if _, ok := deps.sched.Hit("s1"); ok {
		t.Fatal("hit stayed after the person wrote in the chat")
	}
	if _, armed := deps.sched.AutoFor("s1"); armed {
		t.Fatal("the continue stayed armed, it would repeat what they just did")
	}
	// A scheduled turn the person asked for with their own text is not touched.
	mine, _ := deps.sched.Add(protocol.ScheduleTurnData{SessionID: "s1", Text: "mine", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerLimitReset}})
	clearLimitOnSend(deps, "s1")
	if e, _ := deps.sched.Get(mine.ID); e.Status != protocol.SchedulePending {
		t.Fatalf("own scheduled turn = %s", e.Status)
	}
}

func TestContinueRunsAfterRestartWhenTheLimitResetMeanwhile(t *testing.T) {
	deps, pc := limitDeps(t)
	_ = deps.settings.SetLimitAction(protocol.LimitActionContinue)
	root := pc.Root
	// The hit, and its reset time, are already in the past: the server was down across the reset.
	deps.sched.SetHit(protocol.LimitHit{SessionID: "s1", ProjectPath: root, AtMs: time.Now().Add(-5 * time.Hour).UnixMilli(), ResetsAtMs: time.Now().Add(-time.Hour).UnixMilli()}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(ctx, deps)
	rearmLimitHits(deps) // a continue lost in the crash comes back
	deps.sched.Tick()
	deps.turnWG.Wait()
	if _, ok := deps.sched.Hit("s1"); ok {
		t.Fatal("the continue got through but the hit stayed")
	}
	state, err := pc.SessStore.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	var user *string
	for _, m := range state.History {
		if m.Role == "user" {
			c := m.Content
			user = &c
			if m.Origin == nil || m.Origin.Label != protocol.OriginLabelLimit {
				t.Fatalf("origin = %+v", m.Origin)
			}
		}
	}
	if user == nil || !strings.Contains(*user, "usage limit has reset") {
		t.Fatalf("continue text = %v", user)
	}
}

func TestLimitSettingAndAccess(t *testing.T) {
	deps, _ := limitDeps(t)
	if deps.settings.GetLimitAction() != protocol.LimitActionAsk {
		t.Fatal("default is not ask")
	}
	if deps.settings.SetLimitAction("nonsense") == nil {
		t.Fatal("accepted a bad action")
	}
	limited := addPerson(t, deps, TierLimited)
	if remoteDenied(deps, "tcp", limited.ID, protocol.EventLimitContinue) || remoteDenied(deps, "tcp", limited.ID, protocol.EventLimitDismiss) {
		t.Error("a paired phone must be able to answer the card")
	}
	if !remoteDenied(deps, "tcp", limited.ID, protocol.EventSetLimitAction) {
		t.Error("a paired phone can change the limit setting")
	}
}
