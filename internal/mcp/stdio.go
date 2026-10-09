package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// transport carries JSON-RPC messages to one server. call sends a request and
// returns its response; whatever else the server sends meanwhile (progress,
// list-changed notifications, its own requests) goes to the handler.
type transport interface {
	call(ctx context.Context, req *message) (*message, error)
	notify(ctx context.Context, n *message) error
	close()
	// done is closed when the connection is gone (the process exited, the
	// stream ended), so a manager can notice a dead server without calling it.
	done() <-chan struct{}
	// diag is what the server said about itself on stderr, for an error message.
	diag() string
}

// handler answers a message the server sent: nil for a notification or a
// request it doesn't answer, else the reply to send back.
type handler func(m *message) *message

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
	return strings.TrimSpace(strings.ToValidUTF8(string(t.buf), "?"))
}

// stdioTransport runs the server as a child process and speaks newline-
// delimited JSON-RPC on its stdin and stdout.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	h      handler
	stderr tailBuffer

	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan *message
	closed  chan struct{}
	exitErr error
}

// startStdio launches command in dir with exactly env as its environment.
func startStdio(command string, args []string, dir string, env []string, h handler) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = env
	ownProcessGroup(cmd)
	t := &stdioTransport{cmd: cmd, h: h, pending: map[string]chan *message{}, closed: make(chan struct{})}
	cmd.Stderr = &t.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t.stdin = stdin
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", command, err)
	}
	go t.read(stdout)
	return t, nil
}

func (t *stdioTransport) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	for sc.Scan() {
		line := sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		msgs, err := parseMessages(line)
		if err != nil {
			continue // a server that logs to stdout isn't spoken to in anger; skip the line
		}
		for _, m := range msgs {
			t.dispatch(m)
		}
	}
	// stdout is closed: the process is going or gone. Wait reaps it and tells why.
	if sc.Err() != nil {
		killTree(t.cmd)
	}
	err := t.cmd.Wait()
	t.mu.Lock()
	t.exitErr = err
	t.mu.Unlock()
	close(t.closed)
}

func (t *stdioTransport) dispatch(m *message) {
	switch {
	case m.isResponse():
		t.mu.Lock()
		ch := t.pending[idKey(m.ID)]
		delete(t.pending, idKey(m.ID))
		t.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	case m.isRequest():
		go func() { // a request can block (it may wait on a person), so it never holds up the read loop
			if reply := t.h(m); reply != nil {
				_ = t.write(reply)
			}
		}()
	default:
		t.h(m)
	}
}

func (t *stdioTransport) write(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	select {
	case <-t.closed:
		return t.gone()
	default:
	}
	_, err = t.stdin.Write(append(b, '\n'))
	return err
}

func (t *stdioTransport) call(ctx context.Context, req *message) (*message, error) {
	ch := make(chan *message, 1)
	key := idKey(req.ID)
	t.mu.Lock()
	t.pending[key] = ch
	t.mu.Unlock()
	forget := func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}
	if err := t.write(req); err != nil {
		forget()
		return nil, err
	}
	select {
	case m := <-ch:
		return m, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-t.closed:
		forget()
		return nil, t.gone()
	}
}

func (t *stdioTransport) notify(_ context.Context, n *message) error { return t.write(n) }

// gone is the error for a call on a process that has ended: how it ended, and
// the last thing it said about it.
func (t *stdioTransport) gone() error {
	t.mu.Lock()
	err := t.exitErr
	t.mu.Unlock()
	msg := "the server process exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	if d := t.diag(); d != "" {
		msg += "\n" + d
	}
	return errors.New(msg)
}

func (t *stdioTransport) done() <-chan struct{} { return t.closed }
func (t *stdioTransport) diag() string          { return t.stderr.String() }

// close ends the server the way the spec asks: close its stdin, give it a moment
// to leave, then kill it and whatever it started.
func (t *stdioTransport) close() {
	_ = t.stdin.Close()
	select {
	case <-t.closed:
		return
	case <-time.After(2 * time.Second):
	}
	killTree(t.cmd)
	select {
	case <-t.closed:
	case <-time.After(2 * time.Second):
	}
}
