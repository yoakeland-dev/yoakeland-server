package sched

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

type fixture struct {
	s      *Scheduler
	t      time.Time
	ran    []protocol.ScheduledTurn
	ends   []func()
	runErr error
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: time.Unix(1_700_000_000, 0)}
	f.s = New(t.TempDir())
	f.s.now = func() time.Time { return f.t }
	f.s.Run = func(st protocol.ScheduledTurn, finished func()) error {
		if f.runErr != nil {
			return f.runErr
		}
		f.ran = append(f.ran, st)
		f.ends = append(f.ends, finished)
		return nil
	}
	return f
}

func (f *fixture) add(t *testing.T, tr protocol.ScheduleTrigger) protocol.ScheduledTurn {
	t.Helper()
	got, err := f.s.Add(protocol.ScheduleTurnData{SessionID: "s1", Text: "go", Trigger: tr})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *fixture) status(id string) string {
	for _, t := range f.s.List("").Schedules {
		if t.ID == id {
			return t.Status
		}
	}
	return ""
}

func TestNowRunsOnceAndFinishes(t *testing.T) {
	f := newFixture(t)
	a := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerNow})
	f.s.Tick()
	f.s.Tick()
	if len(f.ran) != 1 || f.status(a.ID) != protocol.ScheduleRunning {
		t.Fatalf("ran %d, status %s", len(f.ran), f.status(a.ID))
	}
	if f.ran[0].Origin.Kind != protocol.OriginScheduled || f.ran[0].Origin.ScheduleID != a.ID || f.ran[0].Origin.Trigger != "now" {
		t.Fatalf("origin %+v", f.ran[0].Origin)
	}
	f.ends[0]()
	if f.status(a.ID) != protocol.ScheduleDone {
		t.Fatalf("status %s", f.status(a.ID))
	}
}

func TestAtWaitsForItsTime(t *testing.T) {
	f := newFixture(t)
	f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerAt, AtMs: f.t.Add(time.Hour).UnixMilli()})
	f.s.Tick()
	if len(f.ran) != 0 {
		t.Fatal("ran early")
	}
	f.t = f.t.Add(time.Hour)
	f.s.Tick()
	if len(f.ran) != 1 {
		t.Fatal("did not run when due")
	}
}

func TestRepeatRulesAndCap(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.Add(protocol.ScheduleTurnData{SessionID: "s1", Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerRepeat, EveryMs: 1000}}); err == nil {
		t.Fatal("a 1s repeat was accepted")
	}
	a := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerRepeat, EveryMs: 60_000, MaxRuns: 999})
	if got := f.s.List("").Schedules[0].Trigger.MaxRuns; got != DefaultRepeatRuns {
		t.Fatalf("maxRuns %d", got)
	}
	for i := 0; i < DefaultRepeatRuns; i++ {
		f.t = f.t.Add(time.Minute)
		f.s.Tick()
		if len(f.ran) != i+1 {
			t.Fatalf("run %d: ran %d", i, len(f.ran))
		}
		// While its turn is still going, a repeat doesn't start another.
		f.t = f.t.Add(time.Hour)
		f.s.Tick()
		if len(f.ran) != i+1 {
			t.Fatal("overlapped its own turn")
		}
		f.ends[i]()
	}
	if f.status(a.ID) != protocol.ScheduleDone {
		t.Fatalf("status %s after the cap", f.status(a.ID))
	}
}

func TestPauseCancelAndAllPaused(t *testing.T) {
	f := newFixture(t)
	a := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerNow})
	b := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerNow})
	_ = f.s.Pause(a.ID, false, true)
	_ = f.s.Cancel(b.ID)
	f.s.Tick()
	if len(f.ran) != 0 {
		t.Fatal("a paused or cancelled entry ran")
	}
	_ = f.s.Pause(a.ID, false, false)
	_ = f.s.Pause("", true, true)
	f.s.Tick()
	if len(f.ran) != 0 {
		t.Fatal("ran while everything is paused")
	}
	_ = f.s.Pause("", true, false)
	f.s.Tick()
	if len(f.ran) != 1 || f.ran[0].ID != a.ID {
		t.Fatalf("ran %+v", f.ran)
	}
}

func TestLimitResetWaitsWhileBlocked(t *testing.T) {
	f := newFixture(t)
	blocked := true
	f.s.Blocked = func(protocol.ScheduledTurn) bool { return blocked }
	f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerLimitReset})
	f.s.Tick()
	blocked = false
	f.t = f.t.Add(5 * time.Second) // inside the recheck gap: still treated as held
	f.s.Tick()
	if len(f.ran) != 0 {
		t.Fatal("ran inside the recheck gap")
	}
	f.t = f.t.Add(limitRecheckEvery)
	f.s.Tick()
	if len(f.ran) != 1 {
		t.Fatal("did not run once the limit lifted")
	}
}

func TestRunErrorEndsEntryWithoutLooping(t *testing.T) {
	f := newFixture(t)
	f.runErr = errors.New("no provider")
	a := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerRepeat, EveryMs: 60_000})
	f.t = f.t.Add(time.Minute)
	f.s.Tick()
	got := f.s.List("").Schedules[0]
	if got.ID != a.ID || got.Status != protocol.ScheduleDone || got.Error != "no provider" {
		t.Fatalf("%+v", got)
	}
}

func TestLimitsAndPersistence(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < MaxPerSession; i++ {
		f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerAt, AtMs: f.t.Add(time.Hour).UnixMilli()})
	}
	if _, err := f.s.Add(protocol.ScheduleTurnData{SessionID: "s1", Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}}); err == nil {
		t.Fatal("a 21st pending turn was accepted")
	}
	if _, err := f.s.Add(protocol.ScheduleTurnData{SessionID: "s2", Text: "  ", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}}); err == nil {
		t.Fatal("empty text accepted")
	}
	again := New(f.s.path[:len(f.s.path)-len(fileName)])
	if n := len(again.List("").Schedules); n != MaxPerSession {
		t.Fatalf("reloaded %d", n)
	}
	b, _ := again.Add(protocol.ScheduleTurnData{SessionID: "s2", Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}})
	if b.ID != "sch-21" {
		t.Fatalf("id %s: ids must not repeat after a restart", b.ID)
	}
	again.Forget("s1")
	if n := len(again.List("").Schedules); n != 1 {
		t.Fatalf("after Forget %d", n)
	}
}

func TestRunningEntryDoesNotSurviveARestart(t *testing.T) {
	f := newFixture(t)
	a := f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerNow})
	f.s.Tick() // running, its turn never ends
	again := New(f.s.path[:len(f.s.path)-len(fileName)])
	again.now = f.s.now
	again.Run = func(protocol.ScheduledTurn, func()) error { t.Fatal("re-ran a turn that was interrupted"); return nil }
	again.Tick()
	got := again.List("").Schedules[0]
	if got.ID != a.ID || got.Status != protocol.ScheduleDone || got.Error != restartInterrupted {
		t.Fatalf("%+v", got)
	}
}

// Blocked looks at the rest of the server, and may look at the schedule itself; it mustn't be called
// with the schedule's lock held.
func TestBlockedMayCallBackIntoTheScheduler(t *testing.T) {
	f := newFixture(t)
	f.s.Blocked = func(protocol.ScheduledTurn) bool { f.s.List(""); _, _ = f.s.BindingFor("s1"); return false }
	f.add(t, protocol.ScheduleTrigger{Kind: protocol.TriggerLimitReset})
	done := make(chan struct{})
	go func() { f.s.Tick(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick deadlocked with Blocked calling back in")
	}
	if len(f.ran) != 1 {
		t.Fatal("did not run")
	}
}

func TestRunReturningErrCancelledCancelsTheEntry(t *testing.T) {
	s := New(t.TempDir())
	s.Run = func(protocol.ScheduledTurn, func()) error { return fmt.Errorf("%w: gone", ErrCancelled) }
	e, err := s.AddAs(protocol.ScheduleTurnData{SessionID: "s", Text: "x", Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow}}, "u-1", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if e.CreatorUserID != "u-1" || e.CreatorCredential != "phone" {
		t.Fatalf("creator not stored: %+v", e)
	}
	s.Tick()
	got, _ := s.Get(e.ID)
	if got.Status != protocol.ScheduleCancelled || got.Runs != 0 || got.NextRunAtMs != 0 {
		t.Fatalf("entry = %+v", got)
	}
	s.Tick()
	if got, _ = s.Get(e.ID); got.Status != protocol.ScheduleCancelled {
		t.Fatalf("a cancelled entry ran again: %+v", got)
	}
}
