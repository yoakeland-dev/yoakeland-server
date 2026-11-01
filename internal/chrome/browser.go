package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures Launch.
type Options struct {
	Binary   string // from Ensure
	StateDir string // the profile lives in <StateDir>/browser-profile
	// BlockedPorts are ports on this host's own addresses (loopback and
	// interface IPs) the browser may not connect to: yoakeland's listeners.
	BlockedPorts []uint16
	// Debugf receives Chrome's stderr, line by line and bounded. Nil discards it.
	Debugf func(format string, args ...any)
}

// CDPError is an error response from Chrome.
type CDPError struct {
	Code    int
	Message string
}

func (e *CDPError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

var errClosed = errors.New("browser closed")

type message struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *CDPError       `json:"error"`
	SessionID string          `json:"sessionId"`
}

// Browser is a running headless Chrome driven over --remote-debugging-pipe.
type Browser struct {
	cmd    *exec.Cmd
	proxy  *proxy
	w      *os.File
	wmu    sync.Mutex
	lastID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan message
	subs    map[int]func(sessionID, method string, params json.RawMessage)
	nextSub int

	done      chan struct{}
	err       error // set before done closes
	closeOnce sync.Once
}

// Launch starts Chrome with the sandbox on and every request forced through
// the filtering proxy, and returns once it answers over the pipe.
//
// It refuses to run as root, where Chrome's sandbox can't work, unless
// YOAKE_BROWSER_NO_SANDBOX=1, which also passes --no-sandbox. The sandbox is
// never dropped automatically.
func Launch(ctx context.Context, o Options) (*Browser, error) {
	// ponytail: Windows has no ExtraFiles fd passing; it needs
	// --remote-debugging-io-pipes with inherited handles. Add when a Windows
	// server needs the browser.
	if runtime.GOOS == "windows" {
		return nil, errors.New("headless browser is not supported on Windows yet")
	}
	noSandbox := os.Getenv("YOAKE_BROWSER_NO_SANDBOX") == "1"
	if os.Geteuid() == 0 && !noSandbox {
		return nil, errors.New("refusing to run the browser as root: Chrome's sandbox does not work there; run yoakeland as a normal user, or set YOAKE_BROWSER_NO_SANDBOX=1 to run it unsandboxed")
	}
	profile := filepath.Join(o.StateDir, "browser-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(profile, 0o700); err != nil {
		return nil, err
	}
	px, err := newProxy(o.BlockedPorts)
	if err != nil {
		return nil, err
	}
	toChromeR, toChromeW, err := os.Pipe()
	if err != nil {
		px.close()
		return nil, err
	}
	fromChromeR, fromChromeW, err := os.Pipe()
	if err != nil {
		px.close()
		toChromeR.Close()
		toChromeW.Close()
		return nil, err
	}

	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + profile,
		"--disable-extensions",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-gpu",
		"--proxy-server=socks5://" + px.addr(),
		"--proxy-bypass-list=<-loopback>", // else loopback skips the proxy
	}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")

	lw := &logWriter{logf: o.Debugf}
	cmd := exec.Command(o.Binary, args...)
	cmd.ExtraFiles = []*os.File{toChromeR, fromChromeW} // fds 3 and 4
	cmd.Env = cleanEnv()
	cmd.Stderr = lw
	cmd.WaitDelay = 5 * time.Second
	confine(cmd)
	err = cmd.Start()
	toChromeR.Close() // the child holds its own copies now
	fromChromeW.Close()
	if err != nil {
		px.close()
		toChromeW.Close()
		fromChromeR.Close()
		return nil, fmt.Errorf("starting browser: %w", err)
	}

	b := &Browser{
		cmd: cmd, proxy: px, w: toChromeW,
		pending: map[int64]chan message{},
		subs:    map[int]func(string, string, json.RawMessage){},
		done:    make(chan struct{}),
	}
	go b.readLoop(fromChromeR)
	go func() {
		b.err = cmd.Wait()
		px.close()
		toChromeW.Close()
		close(b.done)
	}()

	if _, err := b.Call(ctx, "", "Browser.getVersion", nil); err != nil {
		b.Close()
		return nil, fmt.Errorf("browser did not start: %w; stderr: %s", err, lw.tailString())
	}
	return b, nil
}

// cleanEnv passes Chrome only what it needs, not the server's secrets.
func cleanEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (b *Browser) readLoop(r *os.File) {
	defer r.Close()
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes(0)
		if err != nil {
			return
		}
		var m message
		if len(line) < 2 || json.Unmarshal(line[:len(line)-1], &m) != nil {
			continue
		}
		b.mu.Lock()
		if m.ID != 0 {
			ch := b.pending[m.ID]
			delete(b.pending, m.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- m // buffered
			}
			continue
		}
		subs := make([]func(string, string, json.RawMessage), 0, len(b.subs))
		for _, fn := range b.subs {
			subs = append(subs, fn)
		}
		b.mu.Unlock()
		for _, fn := range subs {
			fn(m.SessionID, m.Method, m.Params)
		}
	}
}

// Call sends a CDP command and waits for its result. sessionID is empty for
// browser-level commands.
func (b *Browser) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	id := b.lastID.Add(1)
	data, err := json.Marshal(struct {
		ID        int64  `json:"id"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
		SessionID string `json:"sessionId,omitempty"`
	}{id, method, params, sessionID})
	if err != nil {
		return nil, err
	}
	ch := make(chan message, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	// A write that can't finish (Chrome stopped reading fd 3) must not hold
	// wmu forever, and a half-written message corrupts the framing for good.
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(30 * time.Second)
	}
	b.wmu.Lock()
	_ = b.w.SetWriteDeadline(dl)
	_, err = b.w.Write(append(data, 0))
	b.wmu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			_ = b.cmd.Process.Kill()
		}
		return nil, errClosed
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, errClosed
	}
}

// Subscribe registers fn for every CDP event (sessionID is empty for
// browser-level ones) and returns its cancel func. fn runs on the read loop,
// so it must not block or call Call synchronously.
func (b *Browser) Subscribe(fn func(sessionID, method string, params json.RawMessage)) (cancel func()) {
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// Done is closed when the browser process has exited, however it died.
func (b *Browser) Done() <-chan struct{} { return b.done }

// Err is the process exit status once Done is closed.
func (b *Browser) Err() error {
	select {
	case <-b.done:
		return b.err
	default:
		return nil
	}
}

// Close asks Chrome to quit, killing it if it doesn't within a few seconds.
func (b *Browser) Close() {
	b.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sent := make(chan struct{})
		go func() {
			_, _ = b.Call(ctx, "", "Browser.close", nil)
			close(sent)
		}()
		select {
		case <-sent:
		case <-ctx.Done():
		}
		b.w.Close() // EOF on fd 3 also makes Chrome exit; unblocks a stuck write
		select {
		case <-b.done:
		case <-time.After(3 * time.Second):
			_ = b.cmd.Process.Kill()
			<-b.done
		}
	})
}

// logWriter forwards Chrome's stderr to logf, at most 500 lines of 4KB, and
// keeps the last 2KB for error messages.
type logWriter struct {
	logf  func(string, ...any)
	mu    sync.Mutex
	line  []byte
	lines int
	tail  []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = append(w.tail, p...)
	if len(w.tail) > 2048 {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-2048:]...)
	}
	if w.logf == nil {
		return len(p), nil
	}
	for _, c := range p {
		switch {
		case c == '\n':
			w.flush()
		case len(w.line) < 4096:
			w.line = append(w.line, c)
		}
	}
	return len(p), nil
}

func (w *logWriter) flush() {
	switch {
	case w.lines < 500:
		w.logf("chrome: %s", w.line)
	case w.lines == 500:
		w.logf("chrome: stderr log limit reached, dropping the rest")
	}
	w.lines++
	w.line = w.line[:0]
}

func (w *logWriter) tailString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(string(w.tail))
}
