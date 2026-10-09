package terminal

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"yoakeland/internal/protocol"
)

// The agent's side of terminals: it opens ordinary tabs in its own chat (the
// user sees them appear like any other, see protocol.TerminalSummary.Agent),
// types a command into the shell, and reads back what it printed — so a
// long-lived process like a dev server keeps running after the tool call
// that started it returns.

var (
	oscPrompt = []byte("\x1b]633;A")
	oscStart  = []byte("\x1b]633;C")
	oscDone   = []byte("\x1b]633;D")
)

// scanMarks updates ready/busy/finished/lastExit from one output chunk's
// OSC 633 marks, in order. Callers hold t.mu.
// ponytail: a mark split across two pty reads is missed; the agent then just
// waits out its timeout instead of returning early; carry a partial-mark tail
// between reads if that timeout is ever seen in practice.
func (t *Terminal) scanMarks(data []byte) {
	for {
		i := bytes.Index(data, []byte("\x1b]633;"))
		if i < 0 {
			return
		}
		rest := data[i:]
		switch {
		case bytes.HasPrefix(rest, oscPrompt):
			t.ready = true
		case bytes.HasPrefix(rest, oscStart):
			t.busy = true
		case bytes.HasPrefix(rest, oscDone):
			if t.busy {
				t.busy = false
				t.finished++
				code := rest[len(oscDone):]
				if len(code) > 0 && code[0] == ';' {
					code = code[1:]
				}
				if end := bytes.IndexAny(code, "\x07\x1b"); end >= 0 {
					code = code[:end]
				}
				if n, err := strconv.Atoi(string(code)); err == nil {
					t.lastExit = &n
				}
			}
		}
		data = rest[len("\x1b]633;"):]
	}
}

// AgentState is what the agent's terminal tools report about one tab.
type AgentState struct {
	ID       string
	Name     string
	Agent    bool
	Status   protocol.TerminalStatus
	ExitCode *int // the shell's own, once it exited
	Busy     bool // a command is running (shell integration only)
	Finished int  // commands that have ended so far
	LastExit *int // the latest finished command's exit code
	Written  uint64
}

func (t *Terminal) stateLocked() AgentState {
	return AgentState{
		ID: t.id, Name: t.name, Agent: t.agent, Status: t.status, ExitCode: t.exitCode,
		Busy: t.busy, Finished: t.finished, LastExit: t.lastExit, Written: t.written,
	}
}

// State reports id's current state without consuming output.
func (m *Manager) State(sessionID, id string) (AgentState, error) {
	t, ok := m.get(sessionID, id)
	if !ok {
		return AgentState{}, fmt.Errorf("terminal %q not found", id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked(), nil
}

// Read returns what id printed since the agent's last Read (everything, the
// first time), advancing the cursor. skipped counts bytes that scrolled out
// of the history before they were read.
func (m *Manager) Read(sessionID, id string) (out []byte, skipped uint64, st AgentState, err error) {
	t, ok := m.get(sessionID, id)
	if !ok {
		return nil, 0, AgentState{}, fmt.Errorf("terminal %q not found", id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	start := t.written - uint64(len(t.history))
	if t.readAt < start {
		skipped = start - t.readAt
		t.readAt = start
	}
	out = append([]byte(nil), t.history[t.readAt-start:]...)
	t.readAt = t.written
	return out, skipped, t.stateLocked(), nil
}

// Start opens a new agent-owned tab named name in sessionID's chat, waits
// briefly for its shell to draw a prompt, and types command into it. It
// returns the new tab's id. ctx must outlive the call (see Open): pass one
// that isn't cancelled when the agent's turn ends.
func (m *Manager) Start(ctx context.Context, sessionID, name, cwdRel, command string) (string, error) {
	if name == "" {
		name = "terminal"
	}
	var id string
	for n := 1; ; n++ {
		id = fmt.Sprintf("agent-%d", n)
		if _, taken := m.get(sessionID, id); taken {
			continue
		}
		_, err := m.open(ctx, nil, sessionID, id, cwdRel, 120, 32, nil, name)
		if err == nil {
			break
		}
		if _, taken := m.get(sessionID, id); !taken {
			return "", err // a real failure, not a race for the id
		}
	}

	// Typing before the shell's line editor starts gets echoed twice, so wait
	// for the prompt mark — or, for a shell without integration, for its
	// startup output to go quiet.
	deadline := time.Now().Add(3 * time.Second)
	var lastWritten uint64
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		st, err := m.State(sessionID, id)
		if err != nil {
			return "", err
		}
		t, _ := m.get(sessionID, id)
		t.mu.Lock()
		ready := t.ready
		t.mu.Unlock()
		if ready || st.Status != protocol.TerminalStatusRunning {
			break
		}
		if st.Written != lastWritten {
			lastWritten, quietSince = st.Written, time.Now()
		} else if st.Written > 0 && time.Since(quietSince) > 500*time.Millisecond {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := m.Write(sessionID, id, []byte(command+"\n")); err != nil {
		return id, err
	}
	return id, nil
}
