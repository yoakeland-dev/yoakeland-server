//go:build unix

package terminal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
)

func newSweepManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	m := NewManager(sandbox.NewSandbox(dir, 60*time.Second), norm)
	t.Cleanup(func() { m.CloseSession("s") })
	return m
}

func discardStream() *protocol.IPCStream {
	return protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// atPrompt types until the shell has drawn a prompt and the foreground group is recorded.
func atPrompt(t *testing.T, m *Manager, id string) *Terminal {
	t.Helper()
	tm, _ := m.get("s", id)
	waitUntil(t, "a prompt", func() bool {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		return tm.ready && tm.idlePgrp != 0
	})
	return tm
}

func listBusy(m *Manager, id string) (busy, found bool) {
	for _, s := range m.All() {
		if s.TerminalID == id {
			return s.Busy, true
		}
	}
	return false, false
}

// The foreground process group, not the output marks, decides busy: with the marks' own flag
// forced off, a running command still reads as busy and the prompt reads as idle.
func TestBusyFollowsForegroundGroup(t *testing.T) {
	m := newSweepManager(t)
	if _, err := m.Open(context.Background(), discardStream(), "s", "t", "", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	tm := atPrompt(t, m, "t")
	if busy, _ := listBusy(m, "t"); busy {
		t.Fatal("an idle prompt reads as busy")
	}
	if err := m.Write("s", "t", []byte("sleep 30\n")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "busy", func() bool { b, _ := listBusy(m, "t"); return b })
	tm.mu.Lock()
	tm.busy = false // what a lost C mark looks like
	tm.mu.Unlock()
	if b, _ := listBusy(m, "t"); !b {
		t.Fatal("a lost C mark made a running command look idle")
	}
	if err := m.Write("s", "t", []byte{3}); err != nil { // ^C
		t.Fatal(err)
	}
	waitUntil(t, "idle again", func() bool { b, _ := listBusy(m, "t"); return !b })
	tm.mu.Lock()
	tm.busy = true // what a lost D mark looks like
	tm.mu.Unlock()
	if b, _ := listBusy(m, "t"); b {
		t.Fatal("a lost D mark left an idle prompt marked busy")
	}
}

func backdate(tm *Terminal) {
	tm.mu.Lock()
	if !tm.unattachedSince.IsZero() {
		tm.unattachedSince = time.Now().Add(-time.Hour)
	}
	tm.mu.Unlock()
}

func TestSweepForgetsIdleUnwatchedShells(t *testing.T) {
	m := newSweepManager(t)
	st := discardStream()
	if _, err := m.Open(context.Background(), st, "s", "t", "", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	tm := atPrompt(t, m, "t")

	m.sweep()
	backdate(tm)
	m.sweep()
	if _, found := listBusy(m, "t"); !found {
		t.Fatal("a watched shell was forgotten")
	}

	m.Detach(st, "s", "t")
	m.sweep() // starts the clock
	m.sweep() // still inside the grace period
	if _, found := listBusy(m, "t"); !found {
		t.Fatal("forgotten before the grace period ended")
	}
	backdate(tm)
	m.sweep()
	if _, found := listBusy(m, "t"); found {
		t.Fatal("an idle shell nobody watches survived")
	}
}

func TestSweepKeepsWhatIsRunning(t *testing.T) {
	m := newSweepManager(t)
	st := discardStream()
	if _, err := m.Open(context.Background(), st, "s", "t", "", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	tm := atPrompt(t, m, "t")
	if err := m.Write("s", "t", []byte("sleep 30\n")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "busy", func() bool { b, _ := listBusy(m, "t"); return b })
	m.Detach(st, "s", "t")
	m.sweep()
	backdate(tm)
	m.sweep()
	if _, found := listBusy(m, "t"); !found {
		t.Fatal("a shell with a command running was forgotten")
	}

	// Once it ends, the same unwatched shell is just an idle one.
	if err := m.Write("s", "t", []byte{3}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "idle", func() bool { b, _ := listBusy(m, "t"); return !b })
	m.sweep()
	backdate(tm)
	m.sweep()
	if _, found := listBusy(m, "t"); found {
		t.Fatal("a finished, unwatched shell survived")
	}
}

func TestSweepNeverForgetsTheAgentsTabs(t *testing.T) {
	m := newSweepManager(t)
	if _, err := m.open(context.Background(), nil, "s", "agent-1", "", 120, 32, nil, "dev server"); err != nil {
		t.Fatal(err)
	}
	tm, _ := m.get("s", "agent-1")
	for range 3 {
		m.sweep()
		backdate(tm)
	}
	if _, found := listBusy(m, "agent-1"); !found {
		t.Fatal("an agent terminal was swept")
	}
}

func TestDetachStreamDropsAClosedConnection(t *testing.T) {
	m := newSweepManager(t)
	st := discardStream()
	if _, err := m.Open(context.Background(), st, "s", "t", "", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	tm := atPrompt(t, m, "t")
	m.DetachStream(st)
	tm.mu.Lock()
	n := len(tm.subs)
	tm.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d subscribers left after the connection closed", n)
	}
}

func TestUserTerminalsPerChatAreCapped(t *testing.T) {
	m := newSweepManager(t)
	for i := range maxUserTerminals {
		if _, err := m.Open(context.Background(), discardStream(), "s", fmt.Sprintf("t%d", i), "", 0, 0, nil); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	_, err := m.Open(context.Background(), discardStream(), "s", "one-too-many", "", 0, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("got %v, want a cap error", err)
	}
	// The agent's tabs have their own cap.
	if _, err := m.open(context.Background(), nil, "s", "agent-1", "", 120, 32, nil, "x"); err != nil {
		t.Fatalf("agent tab blocked by the user cap: %v", err)
	}
}

// A person's terminal is their own shell (their HOME, no sandbox); the agent's stays confined.
func TestPersonsTerminalRunsOnTheHost(t *testing.T) {
	m := newSweepManager(t)
	snap, err := m.Open(context.Background(), discardStream(), "s", "t", "", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SandboxTier != string(sandbox.ModeHost) {
		t.Fatalf("a person's terminal ran under %q", snap.SandboxTier)
	}
	atPrompt(t, m, "t")
	if err := m.Write("s", "t", []byte("echo home=$((40+2))$HOME\n")); err != nil {
		t.Fatal(err)
	}
	want := "home=42" + os.Getenv("HOME")
	var out []byte
	waitUntil(t, "the shell's HOME", func() bool {
		b, _, _, _ := m.Read("s", "t")
		out = append(out, b...)
		return strings.Contains(string(out), want)
	})

	agent, err := m.open(context.Background(), nil, "s", "agent-1", "", 120, 32, nil, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if agent.SandboxTier == string(sandbox.ModeHost) {
		t.Fatal("the agent's terminal ran outside the sandbox")
	}
}
