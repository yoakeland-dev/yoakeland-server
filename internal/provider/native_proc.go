package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
)

// nativeProc is one running CLI agent: its stdin, a line reader over its
// stdout, and the tail of its stderr (for an error message when it dies).
type nativeProc struct {
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	stderr func() string
	wait   func() error
	kill   func() // nil when the process can't be killed (a test fake)
}

// procStarter starts a CLI agent. Tests substitute one that wires the
// provider to an in-process fake instead of a real binary.
type procStarter func(ctx context.Context, name string, args []string, dir string) (*nativeProc, error)

func startProc(ctx context.Context, name string, args []string, dir string) (*nativeProc, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = cliEnv(os.Environ())
	ownProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var errBuf tailBuffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", name, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxFrameSize)
	return &nativeProc{stdin: stdin, lines: sc, stderr: errBuf.String, wait: cmd.Wait, kill: func() { _ = killTree(cmd) }}, nil
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func writeJSONLine(w io.Writer, mu *sync.Mutex, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	_, err = w.Write(append(b, '\n'))
	return err
}

// rpcConn is a JSON-RPC 2.0 peer over a nativeProc's stdio — the transport
// both `codex app-server` and every ACP agent speak. Notifications are
// handled in order on the read loop; requests from the agent (permission
// asks, which block on a human) each get their own goroutine and answer
// through respond.
type rpcConn struct {
	p         *nativeProc
	wmu       sync.Mutex
	mu        sync.Mutex
	next      int
	pending   map[int]chan rpcReply
	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
	done      chan struct{}
	readErr   error
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Data)
	}
	return e.Message
}

type rpcReply struct {
	result json.RawMessage
	err    *rpcError
}

func newRPCConn(p *nativeProc, onNotify func(string, json.RawMessage), onRequest func(json.RawMessage, string, json.RawMessage)) *rpcConn {
	c := &rpcConn{p: p, pending: map[int]chan rpcReply{}, onNotify: onNotify, onRequest: onRequest, done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *rpcConn) readLoop() {
	for c.p.lines.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(c.p.lines.Bytes(), &msg) != nil {
			continue // stray log line on stdout
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case msg.Method != "" && hasID:
			go c.onRequest(msg.ID, msg.Method, msg.Params)
		case msg.Method != "":
			c.onNotify(msg.Method, msg.Params)
		case hasID:
			var id int
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcReply{result: msg.Result, err: msg.Error}
			}
		}
	}
	c.readErr = c.p.lines.Err()
	close(c.done)
	// Reap the exited child so it doesn't stay a zombie until the daemon exits.
	if c.p.wait != nil {
		_ = c.p.wait()
	}
}

// call sends a request and waits for its response, the process exiting, or
// ctx ending.
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	if err := writeJSONLine(c.p.stdin, &c.wmu, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("%s: %w", method, r.err)
		}
		return r.result, nil
	case <-c.done:
		return nil, c.exitErr(method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *rpcConn) exitErr(method string) error {
	msg := fmt.Sprintf("agent exited before answering %s", method)
	if tail := c.p.stderr(); tail != "" {
		msg += ": " + lastLines(tail, 5)
	}
	return fmt.Errorf("%s", msg)
}

func (c *rpcConn) notify(method string, params any) {
	_ = writeJSONLine(c.p.stdin, &c.wmu, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcConn) respond(id json.RawMessage, result any) {
	_ = writeJSONLine(c.p.stdin, &c.wmu, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *rpcConn) respondError(id json.RawMessage, code int, message string) {
	_ = writeJSONLine(c.p.stdin, &c.wmu, map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: message}})
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// parseUnifiedDiff turns one file's unified diff text (codex's fileChange
// diff) into the FileDiff shape clients already render for yoakeland's own
// edits, hunks included.
func parseUnifiedDiff(path, text string) *diff.FileDiff {
	fd := &diff.FileDiff{FilePath: path, Unified: text}
	var cur *diff.Hunk
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			fd.Hunks = append(fd.Hunks, diff.Hunk{})
			cur = &fd.Hunks[len(fd.Hunks)-1]
			_, _ = fmt.Sscanf(line, "@@ -%d,%d +%d,%d", &cur.OldStart, &cur.OldLines, &cur.NewStart, &cur.NewLines)
		case cur == nil, strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, `\`):
		case line == "":
		default:
			cur.Lines = append(cur.Lines, diff.DiffLine{Type: diff.LineType(line[:1]), Content: line[1:]})
		}
	}
	return fd
}

// cliEnvKeep is what a CLI agent inherits from the daemon, the rest is dropped: these agents run
// model-driven shell commands, so anything left in their environment (API keys, the master key,
// cloud credentials) is one prompt injection from leaving. The agents sign in with their own
// stored login. Names in YOAKELAND_CLI_ENV (comma separated) are passed on too.
var cliEnvKeep = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "LANG": true, "LANGUAGE": true,
	"TERM": true, "TMPDIR": true, "TZ": true, "SSH_AUTH_SOCK": true, "COLORTERM": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	// Windows
	"SYSTEMROOT": true, "SystemRoot": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "PATHEXT": true,
	"COMSPEC": true, "TEMP": true, "TMP": true, "PROGRAMFILES": true, "PROGRAMDATA": true, "HOMEDRIVE": true, "HOMEPATH": true,
}

// cliEnvPrefixes are kept as families: locale, XDG dirs, and each agent's own settings.
var cliEnvPrefixes = []string{"LC_", "XDG_", "CLAUDE_", "CODEX_", "AMP_", "PI_", "VOLTA_", "NVM_", "MISE_", "ASDF_"}

func cliEnv(environ []string) []string {
	extra := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("YOAKELAND_CLI_ENV"), ",") {
		extra[strings.TrimSpace(n)] = true
	}
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		keep := cliEnvKeep[k] || extra[k]
		for _, p := range cliEnvPrefixes {
			keep = keep || strings.HasPrefix(k, p)
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}
