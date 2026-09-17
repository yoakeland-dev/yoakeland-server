package main

import (
	"context"
	"net"
	"sync"
)

// defaultRemoteAddr is where set_remote_access binds the TCP listener when
// the server was started without -listen (so no address was ever
// configured) and a client enables remote access live.
const defaultRemoteAddr = ":7890"

// remoteControl owns the optional TCP listener's live state, so it can be
// started/stopped both at boot (-listen) and at runtime (set_remote_access)
// through the same code path rather than two divergent ones.
type remoteControl struct {
	mu      sync.Mutex
	ln      net.Listener
	stop    context.CancelFunc
	tracker *connTracker

	addr     string
	tlsCert  string
	tlsKey   string
	stateDir string
	deps     connDeps
	settings *ServerSettings
}

// Start binds the TCP listener (if not already running) and begins
// accepting connections on it. A no-op if remote access is already enabled.
func (r *remoteControl) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln != nil {
		return nil
	}

	addr := r.addr
	if addr == "" {
		addr = defaultRemoteAddr
	}

	token := r.settings.GetToken()
	ln, resolvedToken, err := setupTCPListener(addr, r.stateDir, r.tlsCert, r.tlsKey, token)
	if err != nil {
		return err
	}
	if token == "" {
		// Persist the freshly generated token so a "choose a password"
		// flow that never set one explicitly still survives a restart,
		// rather than silently rotating on every boot.
		_ = r.settings.SetToken(resolvedToken)
	}

	lctx, cancel := context.WithCancel(ctx)
	tracker := newConnTracker()
	r.ln = ln
	r.stop = cancel
	r.tracker = tracker
	go acceptLoop(lctx, ln, "tcp", r.deps, r.settings.LookupToken, tracker)
	return nil
}

// Stop closes the TCP listener, if running, and force-closes every
// currently-accepted connection via the tracker Start registered them in.
// That's deliberate: disabling remote access is a security action, and
// leaving already-authenticated remote peers connected would defeat the
// point of it — a connection idling in a blocking read wouldn't otherwise
// notice the listener going away until (if ever) its next read attempt. The
// local unix listener is untouched either way.
func (r *remoteControl) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return
	}
	r.stop()
	_ = r.ln.Close() // don't wait on acceptLoop's own async ctx-cancel watcher to do this
	r.tracker.CloseAll()
	r.ln = nil
	r.tracker = nil
}
