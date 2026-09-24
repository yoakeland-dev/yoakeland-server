package terminal_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/terminal"
)

// syncBuffer lets the test's polling goroutine read what the manager's
// background pump goroutine is concurrently writing (via IPCStream.Send)
// without racing — bytes.Buffer itself isn't safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// decodedTerminalOutput scans raw newline-delimited envelopes for
// terminal_output events and concatenates their decoded (base64) payloads —
// mirroring what a real client does to reassemble a terminal's byte stream.
func decodedTerminalOutput(t *testing.T, raw string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if env.Type != protocol.EventTerminalOutput {
			continue
		}
		var data protocol.TerminalOutputData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("malformed terminal_output data: %v", err)
		}
		chunk, err := base64.StdEncoding.DecodeString(data.Data)
		if err != nil {
			t.Fatalf("terminal_output data is not valid base64: %v", err)
		}
		out.Write(chunk)
	}
	return out.String()
}

// TestManager_OpenInputEchoClose drives a real shell through Open/Input and
// asserts the echoed marker comes back through terminal_output (proving the
// base64 round trip and the StreamSet fan-out both work end to end), then
// Close tears it down and drops it from List.
func TestManager_OpenInputEchoClose(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatalf("normalizer.New: %v", err)
	}
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)

	out := &syncBuffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)

	snap, err := mgr.Open(context.Background(), ipc, "sess-1", "term-1", "", 80, 24, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if snap.Status != protocol.TerminalStatusRunning {
		t.Fatalf("expected status running after a successful spawn, got %q", snap.Status)
	}
	if snap.Pid == 0 {
		t.Fatalf("expected a nonzero pid in the snapshot")
	}

	const marker = "YOAKE_TERMINAL_TEST_MARKER"
	input := base64.StdEncoding.EncodeToString([]byte("echo " + marker + "\n"))
	if err := mgr.Input("sess-1", "term-1", input); err != nil {
		t.Fatalf("Input: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		seen = decodedTerminalOutput(t, out.String())
		if strings.Contains(seen, marker) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(seen, marker) {
		t.Fatalf("marker %q never appeared in decoded terminal output; got %q", marker, seen)
	}

	if err := mgr.Close("sess-1", "term-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if list := mgr.List("sess-1"); len(list) != 0 {
		t.Fatalf("expected no terminals after Close, got %v", list)
	}
	// A second Close is a "not found" error, not a panic/hang — closing an
	// already-closed tab (a client's duplicate click, a race between two
	// peers) must be a clean no-op-ish error, never crash the dispatch loop.
	if err := mgr.Close("sess-1", "term-1"); err == nil {
		t.Fatalf("expected an error closing an already-closed terminal")
	}
}

// TestManager_MultipleTerminalsPerSessionAreIndependent guards two of this
// session's requirements directly: (1) a chat can hold more than one
// terminal tab at once, each independently addressable, and (2) the same
// terminalId in a *different* chat is a completely separate terminal, not a
// collision — terminalId is only unique within a session.
func TestManager_MultipleTerminalsPerSessionAreIndependent(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})
	ctx := context.Background()

	if _, err := mgr.Open(ctx, ipc, "sess-1", "term-1", "", 0, 0, nil); err != nil {
		t.Fatalf("Open sess-1/term-1: %v", err)
	}
	if _, err := mgr.Open(ctx, ipc, "sess-1", "term-2", "", 0, 0, nil); err != nil {
		t.Fatalf("Open sess-1/term-2: %v", err)
	}
	// Same terminalId, different chat — must not collide with sess-1's own
	// "term-1".
	if _, err := mgr.Open(ctx, ipc, "sess-2", "term-1", "", 0, 0, nil); err != nil {
		t.Fatalf("Open sess-2/term-1: %v", err)
	}

	if got := len(mgr.List("sess-1")); got != 2 {
		t.Fatalf("expected 2 terminals in sess-1, got %d", got)
	}
	if got := len(mgr.List("sess-2")); got != 1 {
		t.Fatalf("expected 1 terminal in sess-2, got %d", got)
	}

	// Deleting sess-1's chat must not touch sess-2's terminal.
	mgr.CloseSession("sess-1")
	if got := len(mgr.List("sess-1")); got != 0 {
		t.Fatalf("expected sess-1's terminals gone after CloseSession, got %d", got)
	}
	if got := len(mgr.List("sess-2")); got != 1 {
		t.Fatalf("expected sess-2's terminal to survive sess-1's CloseSession, got %d", got)
	}
	if err := mgr.Input("sess-1", "term-1", base64.StdEncoding.EncodeToString([]byte("x"))); err == nil {
		t.Fatalf("expected sess-1/term-1 to be gone after CloseSession")
	}
}

// TestManager_OpenRejectsInvalidID guards the terminal-id validation that
// stands in for the wire-level input sanitization other event handlers get
// from JSON struct tags alone — a client-chosen id flows straight into
// nothing filesystem-sensitive today, but must still be bounded so a
// pathological id can't grow the terminals map or its own JSON payloads
// unboundedly.
func TestManager_OpenRejectsInvalidID(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})

	if _, err := mgr.Open(context.Background(), ipc, "sess-1", "", "", 0, 0, nil); err == nil {
		t.Fatalf("expected an error for an empty terminal id")
	}
	if _, err := mgr.Open(context.Background(), ipc, "sess-1", strings.Repeat("a", 200), "", 0, 0, nil); err == nil {
		t.Fatalf("expected an error for an over-length terminal id")
	}
	if _, err := mgr.Open(context.Background(), ipc, "", "term-1", "", 0, 0, nil); err == nil {
		t.Fatalf("expected an error for an empty sessionId")
	}
}

// TestManager_OpenRejectsEscapingCwd guards the same project-root boundary
// file_read/file_write/file_edit already enforce (normalizer.ErrEscapesRoot)
// — a terminal's cwd must not be a backdoor around it.
func TestManager_OpenRejectsEscapingCwd(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})

	if _, err := mgr.Open(context.Background(), ipc, "sess-1", "term-1", "../../etc", 0, 0, nil); err == nil {
		t.Fatalf("expected an error for a cwd escaping the project root")
	}
}

// blockingWriter never returns from Write, simulating a peer whose socket
// write stalls (as opposed to one that's cleanly closed and fails outright)
// — the case the per-subscriber output fan-out exists to isolate.
type blockingWriter struct{}

func (blockingWriter) Write(p []byte) (int, error) {
	select {}
}

// TestManager_StalledSubscriberDoesNotStallOthers guards the flow-control
// fix directly: two peers (a split view / second device) attached to the
// same terminal, one of them permanently stuck mid-write. Before
// per-subscriber fan-out, appendOutput broadcast through one shared
// StreamSet.SendEvent that wrote to each subscriber sequentially on pump's
// own goroutine — a stalled write there would have starved every other
// subscriber, not just the stalled one, since pump never reaches its next
// pty Read until the whole broadcast call returns.
func TestManager_StalledSubscriberDoesNotStallOthers(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 60*time.Second)
	mgr := terminal.NewManager(sb, norm)
	ctx := context.Background()

	opener := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})
	if _, err := mgr.Open(ctx, opener, "sess-1", "term-1", "", 80, 24, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}

	slow := protocol.NewIPCStream(bytes.NewBuffer(nil), blockingWriter{})
	if _, err := mgr.Attach(ctx, slow, "sess-1", "term-1", 0, 0, false); err != nil {
		t.Fatalf("Attach (slow): %v", err)
	}

	fastOut := &syncBuffer{}
	fast := protocol.NewIPCStream(bytes.NewBuffer(nil), fastOut)
	if _, err := mgr.Attach(ctx, fast, "sess-1", "term-1", 0, 0, false); err != nil {
		t.Fatalf("Attach (fast): %v", err)
	}

	const marker = "YOAKE_FLOW_CONTROL_TEST_MARKER"
	// Enough chunks to overflow outputQueueDepth for the slow subscriber
	// several times over.
	script := "for i in $(seq 1 500); do echo line-$i-filler-filler-filler; done; echo " + marker + "\n"
	input := base64.StdEncoding.EncodeToString([]byte(script))
	if err := mgr.Input("sess-1", "term-1", input); err != nil {
		t.Fatalf("Input: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		seen = decodedTerminalOutput(t, fastOut.String())
		if strings.Contains(seen, marker) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(seen, marker) {
		t.Fatalf("fast subscriber never saw marker %q despite a stalled peer sharing this terminal; got %q", marker, seen)
	}
}

// All spans every chat, and OnChange fires on start, exit and close — what
// the server-wide terminal_index is built from and pushed on.
func TestManager_AllAndOnChange(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	mgr := terminal.NewManager(sandbox.NewSandbox(tempDir, 60*time.Second), norm)
	var changes atomic.Int32
	mgr.SetOnChange(func() { changes.Add(1) })
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &syncBuffer{})

	for _, sess := range []string{"sess-a", "sess-b"} {
		if _, err := mgr.Open(context.Background(), ipc, sess, "term-1", "", 0, 0, nil); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	all := mgr.All()
	if len(all) != 2 || all[0].SessionID != "sess-a" || all[1].SessionID != "sess-b" {
		t.Fatalf("All: %+v", all)
	}
	if list := mgr.List("sess-a"); len(list) != 1 || list[0].SessionID != "" {
		t.Fatalf("List keeps its old shape: %+v", list)
	}
	if n := changes.Load(); n != 2 {
		t.Fatalf("changes after two opens = %d", n)
	}

	_ = mgr.Input("sess-a", "term-1", base64.StdEncoding.EncodeToString([]byte("exit 3\n")))
	deadline := time.Now().Add(5 * time.Second)
	for changes.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if a := mgr.All()[0]; a.Status != protocol.TerminalStatusExited || a.ExitCode == nil || *a.ExitCode != 3 {
		t.Fatalf("after exit: %+v (changes %d)", a, changes.Load())
	}

	_ = mgr.Close("sess-b", "term-1")
	if n := changes.Load(); n != 4 || len(mgr.All()) != 1 {
		t.Fatalf("after close: changes %d, all %+v", n, mgr.All())
	}
}
