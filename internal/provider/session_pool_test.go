package provider

import (
	"bufio"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/agent"
)

type fakeSess struct {
	closed atomic.Bool
	gone   atomic.Bool
}

func (f *fakeSess) close()     { f.closed.Store(true) }
func (f *fakeSess) dead() bool { return f.gone.Load() }

func poolTurn() *agent.NativeTurn { return &agent.NativeTurn{SessionKey: "chat"} }

func always(nativeSession) bool { return true }

func resetPool() {
	nativePool.mu.Lock()
	nativePool.live = map[string]*pooled{}
	nativePool.mu.Unlock()
}

func TestPoolSigChangeDoesNotKillBusyTurn(t *testing.T) {
	resetPool()
	first := &fakeSess{}
	_, _, release, err := acquireSession(poolTurn(), CLIKindClaude, "a", always, func() (nativeSession, error) { return first, nil })
	if err != nil {
		t.Fatal(err)
	}
	// A second acquire with a different sig replaces the entry while the first turn runs.
	second := &fakeSess{}
	_, reused, release2, err := acquireSession(poolTurn(), CLIKindClaude, "b", always, func() (nativeSession, error) { return second, nil })
	if err != nil || reused {
		t.Fatal(err, reused)
	}
	if first.closed.Load() {
		t.Fatal("a session with a turn in flight was closed")
	}
	release() // last release of the evicted one closes it
	if !first.closed.Load() {
		t.Fatal("evicted session leaked after its turn ended")
	}
	release2()
	if second.closed.Load() {
		t.Fatal("live session closed on release")
	}
}

func TestPoolLaunchRaceKeepsOne(t *testing.T) {
	resetPool()
	winner, loser := &fakeSess{}, &fakeSess{}
	var rel func()
	// The first launch, while running, lets a rival acquire and win the slot.
	s, _, release, err := acquireSession(poolTurn(), CLIKindClaude, "a", always, func() (nativeSession, error) {
		_, _, r, err := acquireSession(poolTurn(), CLIKindClaude, "a", always, func() (nativeSession, error) { return winner, nil })
		rel = r
		return loser, err
	})
	if err != nil || s != nativeSession(winner) || !loser.closed.Load() || winner.closed.Load() {
		t.Fatalf("err=%v got winner=%v loserClosed=%v", err, s == nativeSession(winner), loser.closed.Load())
	}
	release()
	rel()
}

func TestPoolPrunesDeadIdle(t *testing.T) {
	resetPool()
	f := &fakeSess{}
	_, _, release, _ := acquireSession(poolTurn(), CLIKindClaude, "a", always, func() (nativeSession, error) { return f, nil })
	release()
	f.gone.Store(true)
	nativePool.closeIdle(time.Hour)
	if !f.closed.Load() || liveSession(poolTurn(), CLIKindClaude) != nil {
		t.Fatal("dead idle session not pruned")
	}
}

// A codex/ACP child must be waited on once its stdout ends, or it stays a zombie.
func TestRPCConnReapsExitedChild(t *testing.T) {
	outR, outW := io.Pipe()
	waited := make(chan struct{})
	p := &nativeProc{stdin: nopWriteCloser{io.Discard}, lines: bufio.NewScanner(outR), stderr: func() string { return "" }, wait: func() error { close(waited); return nil }}
	c := newRPCConn(p, func(string, json.RawMessage) {}, func(json.RawMessage, string, json.RawMessage) {})
	outW.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("done not closed")
	}
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("child never waited on")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestPoolCapEvictsLeastRecentlyUsedIdle(t *testing.T) {
	resetPool()
	SetPoolLimits(0, 2)
	defer SetPoolLimits(0, 0)
	mk := func(chat string) (*fakeSess, func()) {
		f := &fakeSess{}
		_, _, rel, err := acquireSession(&agent.NativeTurn{SessionKey: chat}, CLIKindClaude, "a", always, func() (nativeSession, error) { return f, nil })
		if err != nil {
			t.Fatal(err)
		}
		return f, rel
	}
	a, relA := mk("a")
	relA()
	time.Sleep(5 * time.Millisecond)
	b, relB := mk("b") // stays busy
	c, relC := mk("c") // over the cap of 2: a is the LRU idle one
	if !a.closed.Load() || b.closed.Load() || c.closed.Load() {
		t.Fatalf("a=%v b=%v c=%v, want only a evicted", a.closed.Load(), b.closed.Load(), c.closed.Load())
	}
	// Everything busy: the cap is soft and nothing is killed.
	d, relD := mk("d")
	if b.closed.Load() || c.closed.Load() || d.closed.Load() {
		t.Fatal("a busy session was evicted")
	}
	if st := NativePoolStats(); st.Live != 3 || st.Busy != 3 || st.Max != 2 {
		t.Fatalf("stats = %+v", st)
	}
	relB()
	relC()
	relD()
}
