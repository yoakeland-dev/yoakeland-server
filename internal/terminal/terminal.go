// Package terminal implements the built-in interactive terminal feature: a
// real shell a human peer drives (tabs, split views), as opposed to the bash
// tool's single-command-no-shell design for the LLM. It runs through the
// same sandbox.Sandbox tier as the bash tool (see sandbox.PTYSession) so a
// remote TCP peer getting an interactive shell on this host is bound by the
// same isolation ladder as an agent-issued command, not a bigger hole next
// to it.
//
// Terminals are scoped to a chat session, not a project: a chat can have
// several tabs open (each its own Terminal, keyed by a client-chosen
// TerminalID), and deleting the chat tears all of them down — see
// Manager.CloseSession, called from cmd/yoakeland's delete_session handler
// and from project removal, so a terminal never outlives the chat that
// opened it.
package terminal

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
)

const (
	defaultCols = 80
	defaultRows = 24
	maxCols     = 1000
	maxRows     = 500

	// historyLimit bounds the scrollback replayed to a client on attach —
	// ponytail: a fixed byte cap per terminal, not a true VT-aware screen
	// model (what a client-side xterm.js buffer already reconstructs from
	// replayed bytes); bump this or move to persisted history if very long
	// scrollback across restarts ever matters.
	historyLimit = 256 * 1024

	maxInputBytes  = 64 * 1024
	maxEnvVars     = 128
	maxEnvValueLen = 8192
)

var (
	terminalIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	envKeyPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Terminal is one tab: a single interactive shell and everything a client
// needs to repaint it (status, scrollback, size) plus the set of connections
// currently watching its output live.
type Terminal struct {
	mu sync.Mutex

	sessionID string
	id        string
	cwd       string
	cols      int
	rows      int
	env       []string // extra env beyond the sandbox's own base, kept for restart

	status   protocol.TerminalStatus
	pid      int
	exitCode *int
	label    string

	session sandbox.PTYSession

	history  []byte
	sequence uint64
	// streams carries the low-frequency control-plane events (snapshot
	// membership, terminal_exited, terminal_closed) — see subs for the
	// separately fanned-out output hot path.
	streams *protocol.StreamSet
	// subs is the per-connection terminal_output fan-out, keyed by the same
	// *protocol.IPCStream identity as streams. Kept separate from streams
	// (rather than just calling streams.SendEvent from appendOutput) so one
	// slow/stalled subscriber's blocking socket write can't hold up delivery
	// to every other peer attached to this same terminal — see
	// addOutputSubscriberLocked's doc comment.
	subs map[*protocol.IPCStream]*outputSubscriber
}

// outputChunk pairs one appendOutput chunk with the sequence number it was
// assigned, so a subscriber goroutine consuming it later (possibly out of
// step with other subscribers, see outputQueueDepth) still reports the
// sequence its own client expects.
type outputChunk struct {
	data []byte
	seq  uint64
}

// outputSubscriber pumps one attached connection's terminal_output events
// through its own small buffered channel + dedicated goroutine, decoupling
// appendOutput's hot path (called from Terminal.pump, the pty's sole
// reader) from that connection's own write latency.
type outputSubscriber struct {
	stream *protocol.IPCStream
	ch     chan outputChunk
}

// outputQueueDepth bounds how many chunks (each up to pump's 32KB read
// buffer) a subscriber may lag behind before it's cut loose — see
// addOutputSubscriberLocked. ponytail: a fixed guess, not measured against
// real traffic; raise it if a merely-slow-but-alive peer (a laggy WAN TCP
// client, not a dead one) gets dropped too eagerly.
const outputQueueDepth = 64

// attachLocked subscribes stream and returns the current snapshot as one
// atomic step under t.mu — the same lock appendOutput mutates history/
// sequence under, so a client can never miss output that lands in the gap
// between "start watching" and "read current state" (a real gap would be
// unrecoverable; the client has no way to ask for "everything since X" other
// than the sequence number this snapshot itself hands back). The mirror
// case — output landing exactly here and being both included in History and
// re-delivered once more as a live event a moment later — is possible since
// broadcasting happens after this lock is released (see appendOutput); left
// as a rare, harmless duplicate-render rather than serializing every output
// chunk's network write behind this lock too.
func (t *Terminal) attachLocked(stream *protocol.IPCStream, sandboxTier string) *protocol.TerminalSnapshotData {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streams.Add(stream)
	t.addOutputSubscriberLocked(stream)
	return t.snapshotLocked(sandboxTier)
}

// addOutputSubscriberLocked starts (or, for a stream already subscribed —
// including one re-attaching after being dropped for falling behind, see
// dropOutputSubscriber — restarts) the goroutine that delivers this
// connection's terminal_output events. Callers hold t.mu.
func (t *Terminal) addOutputSubscriberLocked(stream *protocol.IPCStream) {
	if _, exists := t.subs[stream]; exists {
		return
	}
	if t.subs == nil {
		t.subs = make(map[*protocol.IPCStream]*outputSubscriber)
	}
	sub := &outputSubscriber{stream: stream, ch: make(chan outputChunk, outputQueueDepth)}
	t.subs[stream] = sub
	id := t.id
	go func() {
		for oc := range sub.ch {
			err := stream.SendEvent(protocol.EventTerminalOutput, "", "", "", protocol.TerminalOutputData{
				TerminalID: id,
				Data:       base64.StdEncoding.EncodeToString(oc.data),
				Sequence:   oc.seq,
			})
			if err != nil {
				// A genuinely dead connection — drop it from both the
				// output fan-out and the control-plane StreamSet, same as
				// StreamSet.SendEvent already does for its own subscribers
				// on a write failure.
				t.streams.Remove(stream)
				t.dropOutputSubscriber(stream)
				return
			}
		}
	}()
}

// dropOutputSubscriber removes stream from the output fan-out and stops its
// goroutine — called either because its channel is full (see appendOutput;
// the client sees this as the same "gap in sequence" case terminal_output's
// own doc already tells it to recover from via a fresh terminal_attach,
// no wire protocol change needed) or because delivery to it failed outright.
// The map-delete doubles as a single-winner gate against a concurrent
// double-close, since appendOutput can race this from multiple chunks.
func (t *Terminal) dropOutputSubscriber(stream *protocol.IPCStream) {
	t.mu.Lock()
	sub, ok := t.subs[stream]
	if ok {
		delete(t.subs, stream)
	}
	t.mu.Unlock()
	if ok {
		close(sub.ch)
	}
}

// closeOutputSubscribers stops every subscriber goroutine still running —
// called once from closeTerminal so a torn-down Terminal doesn't leak them.
func (t *Terminal) closeOutputSubscribers() {
	t.mu.Lock()
	subs := t.subs
	t.subs = nil
	t.mu.Unlock()
	for _, sub := range subs {
		close(sub.ch)
	}
}

func (t *Terminal) snapshotLocked(sandboxTier string) *protocol.TerminalSnapshotData {
	var exitCode *int
	if t.exitCode != nil {
		ec := *t.exitCode
		exitCode = &ec
	}
	return &protocol.TerminalSnapshotData{
		TerminalID:  t.id,
		Cwd:         t.cwd,
		Cols:        t.cols,
		Rows:        t.rows,
		Status:      t.status,
		Pid:         t.pid,
		History:     base64.StdEncoding.EncodeToString(t.history),
		Sequence:    t.sequence,
		ExitCode:    exitCode,
		Label:       t.label,
		SandboxTier: sandboxTier,
	}
}

// pump reads the pty until it hits EOF/error (the shell exited or the
// session was Close()d), fanning each chunk out live, then reaps the process
// and announces its exit. One goroutine per terminal, started right after a
// successful spawn; it's the sole reader of session, and the sole caller of
// session.Wait (see sandbox.PTYSession's doc comment on why that matters —
// calling Wait more than once, or concurrently with the read loop still
// running, is undefined).
func (t *Terminal) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.session.Read(buf)
		if n > 0 {
			t.appendOutput(buf[:n])
		}
		if err != nil {
			break
		}
	}

	exitCode, _ := t.session.Wait()

	t.mu.Lock()
	t.status = protocol.TerminalStatusExited
	t.exitCode = &exitCode
	streams := t.streams
	id := t.id
	t.mu.Unlock()

	_ = streams.SendEvent(protocol.EventTerminalExited, "", "", "", protocol.TerminalExitedData{
		TerminalID: id,
		ExitCode:   exitCode,
	})
}

// appendOutput is pump's only caller, and pump reuses its read buffer across
// iterations — chunk aliases that buffer, so it must be copied before any
// use that outlives this call (the per-subscriber channels below hand it to
// goroutines that read it later, possibly well after pump's next Read
// overwrites the original bytes).
func (t *Terminal) appendOutput(chunk []byte) {
	data := append([]byte(nil), chunk...)

	t.mu.Lock()
	t.history = append(t.history, data...)
	if len(t.history) > historyLimit {
		t.history = t.history[len(t.history)-historyLimit:]
	}
	t.sequence++
	oc := outputChunk{data: data, seq: t.sequence}
	subs := make([]*outputSubscriber, 0, len(t.subs))
	for _, sub := range t.subs {
		subs = append(subs, sub)
	}
	t.mu.Unlock()

	// Non-blocking per subscriber: appendOutput runs on pump's hot loop (the
	// pty's sole reader), so one subscriber's full channel must never stall
	// delivery to the others — see outputSubscriber's doc comment.
	for _, sub := range subs {
		select {
		case sub.ch <- oc:
		default:
			t.dropOutputSubscriber(sub.stream)
		}
	}
}

// Manager owns every terminal (tab) open for one project, grouped by the
// chat session that owns them — the project-scoped counterpart to
// agent.Registry, except a session here can hold several terminals (its
// open tabs) rather than one Agent. TerminalID only needs to be unique
// within a session, not project-wide: two different chats can each have
// their own "term-1", the same convention t3.chat's terminal panel uses
// (its terminals are namespaced by threadId for the same reason).
type Manager struct {
	mu sync.Mutex
	// terminals[sessionID][terminalID] — an empty inner map is pruned away
	// as soon as its last terminal is removed (Close/CloseSession), so
	// "does this session have any terminals" is just a map lookup, not a
	// lookup-then-check-empty.
	terminals map[string]map[string]*Terminal

	sb          sandbox.Sandbox
	norm        *normalizer.Normalizer
	sandboxTier string

	onChange atomic.Pointer[func()]
}

// SetOnChange registers fn to run (with no Manager or Terminal lock held)
// whenever a terminal starts, exits or closes.
func (m *Manager) SetOnChange(fn func()) {
	m.onChange.Store(&fn)
}

func (m *Manager) changed() {
	if fn := m.onChange.Load(); fn != nil {
		(*fn)()
	}
}

func NewManager(sb sandbox.Sandbox, norm *normalizer.Normalizer) *Manager {
	return &Manager{
		terminals:   make(map[string]map[string]*Terminal),
		sb:          sb,
		norm:        norm,
		sandboxTier: string(sb.Mode()),
	}
}

// Open starts a brand-new terminal under id within sessionID's chat, which
// must not already have a tab open under that id — re-opening a live or
// exited-but-not-closed tab is what Attach is for. ctx is expected to be the
// server's own long-lived root context (cancelled only on shutdown, not
// per-connection — see cmd/yoakeland's dispatch loop), so a terminal
// survives the request that opened it and keeps running until the shell
// exits, an explicit terminal_close, or the owning chat is deleted (see
// CloseSession).
func (m *Manager) Open(ctx context.Context, stream *protocol.IPCStream, sessionID, id, cwdRel string, cols, rows int, env map[string]string) (*protocol.TerminalSnapshotData, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("terminal requires a sessionId")
	}
	if !terminalIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid terminal id %q", id)
	}

	extraEnv, err := validateEnv(env)
	if err != nil {
		return nil, err
	}

	cwd := m.norm.ProjectRoot
	if cwdRel != "" {
		resolved, err := m.norm.ResolveAndValidatePath(cwdRel)
		if err != nil {
			return nil, fmt.Errorf("cwd: %w", err)
		}
		cwd = resolved
	}

	m.mu.Lock()
	if _, exists := m.terminals[sessionID][id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("terminal %q is already open", id)
	}
	t := &Terminal{
		sessionID: sessionID,
		id:        id,
		cwd:       cwd,
		cols:      clampCols(cols),
		rows:      clampRows(rows),
		env:       extraEnv,
		status:    protocol.TerminalStatusStarting,
		streams:   protocol.NewStreamSet(),
	}
	if m.terminals[sessionID] == nil {
		m.terminals[sessionID] = make(map[string]*Terminal)
	}
	m.terminals[sessionID][id] = t
	m.mu.Unlock()

	if err := m.spawn(ctx, t); err != nil {
		m.mu.Lock()
		delete(m.terminals[sessionID], id)
		if len(m.terminals[sessionID]) == 0 {
			delete(m.terminals, sessionID)
		}
		m.mu.Unlock()
		return nil, err
	}

	return t.attachLocked(stream, m.sandboxTier), nil
}

// Attach subscribes stream to an already-open terminal in sessionID's chat
// (another tab/device/peer opened it, or this same client is reattaching),
// optionally resizing it and optionally respawning it first if its shell
// already exited.
func (m *Manager) Attach(ctx context.Context, stream *protocol.IPCStream, sessionID, id string, cols, rows int, restartIfExited bool) (*protocol.TerminalSnapshotData, error) {
	t, ok := m.get(sessionID, id)
	if !ok {
		return nil, fmt.Errorf("terminal %q not found", id)
	}

	t.mu.Lock()
	needsRestart := restartIfExited && t.status != protocol.TerminalStatusRunning && t.status != protocol.TerminalStatusStarting
	if cols > 0 {
		t.cols = clampCols(cols)
	}
	if rows > 0 {
		t.rows = clampRows(rows)
	}
	t.mu.Unlock()

	if needsRestart {
		if err := m.spawn(ctx, t); err != nil {
			return nil, err
		}
	}

	return t.attachLocked(stream, m.sandboxTier), nil
}

// spawn launches (or relaunches) t's shell and starts its output pump.
// Callers hold no lock across this call.
func (m *Manager) spawn(ctx context.Context, t *Terminal) error {
	argv := shellArgv()

	t.mu.Lock()
	cols, rows, env, cwd := t.cols, t.rows, t.env, t.cwd
	t.mu.Unlock()

	argv, env = applyShellIntegration(argv, env)
	// Later entries win, so this overrides the sandbox's PWD (the project root).
	env = append(env, "PWD="+cwd)

	session, err := m.sb.SpawnPTY(ctx, argv, env, cwd, cols, rows)
	if err != nil {
		t.mu.Lock()
		t.status = protocol.TerminalStatusError
		t.mu.Unlock()
		m.changed()
		return fmt.Errorf("spawning terminal: %w", err)
	}

	t.mu.Lock()
	t.session = session
	t.pid = session.Pid()
	t.status = protocol.TerminalStatusRunning
	t.exitCode = nil
	t.label = filepath.Base(argv[0])
	t.mu.Unlock()

	go func() {
		t.pump()
		m.changed()
	}()
	m.changed()
	return nil
}

// Input writes base64-encoded bytes to id's pty within sessionID's chat (see
// protocol.TerminalInputData's doc comment for why base64).
func (m *Manager) Input(sessionID, id, dataB64 string) error {
	if len(dataB64) > maxInputBytes*2 { // base64 inflates ~4/3; generous cap before even decoding
		return fmt.Errorf("input too large")
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return fmt.Errorf("invalid base64 input: %w", err)
	}

	t, ok := m.get(sessionID, id)
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}

	t.mu.Lock()
	session := t.session
	running := t.status == protocol.TerminalStatusRunning
	t.mu.Unlock()
	if !running || session == nil {
		return fmt.Errorf("terminal %q is not running", id)
	}

	_, err = session.Write(raw)
	return err
}

func (m *Manager) Resize(sessionID, id string, cols, rows int) error {
	t, ok := m.get(sessionID, id)
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}

	t.mu.Lock()
	t.cols = clampCols(cols)
	t.rows = clampRows(rows)
	session := t.session
	c, r := t.cols, t.rows
	t.mu.Unlock()

	if session == nil {
		return nil
	}
	return session.Resize(c, r)
}

// Close ends id permanently — the shell process (if still running) and the
// tab itself. Unlike a shell simply exiting (see pump), this drops the
// terminal from the registry: a client should treat terminal_closed as
// "this tab is gone," not "reattach or restart it."
func (m *Manager) Close(sessionID, id string) error {
	t, ok := m.remove(sessionID, id)
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}
	closeTerminal(t)
	m.changed()
	return nil
}

// CloseSession tears down every terminal open under sessionID — the shell
// processes and the tabs themselves — and is what makes "delete this chat"
// actually kill its terminals instead of leaking them as ownerless
// background processes. Safe to call for a session with no terminals (a
// no-op) since a deleted/never-opened chat is the common case.
func (m *Manager) CloseSession(sessionID string) {
	m.mu.Lock()
	byID := m.terminals[sessionID]
	delete(m.terminals, sessionID)
	m.mu.Unlock()

	for _, t := range byID {
		closeTerminal(t)
	}
	if len(byID) > 0 {
		m.changed()
	}
}

// closeTerminal does the actual teardown shared by Close and CloseSession —
// callers have already removed t from the Manager's own map.
func closeTerminal(t *Terminal) {
	t.mu.Lock()
	session := t.session
	streams := t.streams
	id := t.id
	t.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
	// pump's own read loop, if still running, observes the closed session
	// as an EOF/error and exits on its own (see pump) — no need to wait for
	// it before telling clients the tab is gone.
	t.closeOutputSubscribers()

	_ = streams.SendEvent(protocol.EventTerminalClosed, "", "", "", protocol.TerminalClosedData{TerminalID: id})
}

// List summarizes every open terminal (tab) in sessionID's chat — sorted by
// id for a stable tab order.
func (m *Manager) List(sessionID string) []protocol.TerminalSummary {
	m.mu.Lock()
	terminals := make([]*Terminal, 0, len(m.terminals[sessionID]))
	for _, t := range m.terminals[sessionID] {
		terminals = append(terminals, t)
	}
	m.mu.Unlock()
	out := summarize(terminals)
	for i := range out {
		out[i].SessionID = ""
	}
	return out
}

// All summarizes every terminal in every chat, with SessionID set, sorted by
// session then id.
func (m *Manager) All() []protocol.TerminalSummary {
	m.mu.Lock()
	var terminals []*Terminal
	for _, byID := range m.terminals {
		for _, t := range byID {
			terminals = append(terminals, t)
		}
	}
	m.mu.Unlock()
	return summarize(terminals)
}

func summarize(terminals []*Terminal) []protocol.TerminalSummary {
	out := make([]protocol.TerminalSummary, 0, len(terminals))
	for _, t := range terminals {
		t.mu.Lock()
		out = append(out, protocol.TerminalSummary{
			TerminalID: t.id,
			Cwd:        t.cwd,
			Status:     t.status,
			Label:      t.label,
			ExitCode:   t.exitCode,
			SessionID:  t.sessionID,
		})
		t.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].TerminalID < out[j].TerminalID
	})
	return out
}

func (m *Manager) get(sessionID, id string) (*Terminal, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.terminals[sessionID][id]
	return t, ok
}

// remove atomically takes ownership of a terminal (removing it, and its now-
// possibly-empty session bucket, from the map) so exactly one caller ever
// proceeds to tear it down, even under a concurrent double terminal_close.
func (m *Manager) remove(sessionID, id string) (*Terminal, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.terminals[sessionID][id]
	if !ok {
		return nil, false
	}
	delete(m.terminals[sessionID], id)
	if len(m.terminals[sessionID]) == 0 {
		delete(m.terminals, sessionID)
	}
	return t, true
}

func clampCols(v int) int {
	if v <= 0 {
		return defaultCols
	}
	if v > maxCols {
		return maxCols
	}
	return v
}

func clampRows(v int) int {
	if v <= 0 {
		return defaultRows
	}
	if v > maxRows {
		return maxRows
	}
	return v
}

// validateEnv turns a client-supplied env map into "KEY=value" pairs
// appended on top of the sandbox's own base environment, with the same
// restricted shape (name pattern, count, value size) node-pty-based tools
// apply for the same reason: an unbounded env is an easy way to blow past
// exec's argv/env size limit or smuggle in something odd via a crafted key.
func validateEnv(env map[string]string) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	if len(env) > maxEnvVars {
		return nil, fmt.Errorf("too many env vars (max %d)", maxEnvVars)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		if !envKeyPattern.MatchString(k) {
			return nil, fmt.Errorf("invalid env var name %q", k)
		}
		if len(v) > maxEnvValueLen {
			return nil, fmt.Errorf("env var %q exceeds %d bytes", k, maxEnvValueLen)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// shellArgv picks the interactive shell to spawn: the daemon's own $SHELL
// (whatever the operator's account is configured with), falling back to
// bash then the POSIX-guaranteed sh — or, on Windows (which has neither
// concept), PowerShell if installed, else the always-present cmd.exe. Never
// client-chosen — unlike the bash tool's argv (a single vetted command with
// no shell interpretation), a terminal's whole point is a real shell;
// letting a client pick the executable instead would turn "open a terminal"
// into "run any binary on this host," a meaningfully bigger capability than
// what's being asked for.
func shellArgv() []string {
	if runtime.GOOS == "windows" {
		return windowsShellArgv()
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return []string{sh}
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return []string{candidate}
		}
	}
	return []string{"/bin/sh"}
}

// windowsShellArgv prefers PowerShell (pwsh.exe, then the older built-in
// powershell.exe) — the nicer default most Windows terminal UIs (VS Code
// included) reach for — before falling back to $COMSPEC or the always-
// present cmd.exe, which never requires a PATH lookup to find.
func windowsShellArgv() []string {
	for _, candidate := range []string{"pwsh.exe", "powershell.exe"} {
		if lp, err := exec.LookPath(candidate); err == nil {
			return []string{lp}
		}
	}
	if cs := os.Getenv("COMSPEC"); cs != "" {
		if _, err := os.Stat(cs); err == nil {
			return []string{cs}
		}
	}
	return []string{"cmd.exe"}
}
