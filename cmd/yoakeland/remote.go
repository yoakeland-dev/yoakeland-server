package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"sync"

	"yoakeland/internal/tlsca"
)

// defaultRemoteAddr is where set_remote_access binds the TCP listener when no
// address has been configured (RemoteSettings.Addr) and a client enables
// remote access.
const defaultRemoteAddr = ":7890"

// remoteControl owns the optional TCP listener's live state, so it can be
// started/stopped both at boot (when RemoteSettings.Enabled was persisted) and
// at runtime (set_remote_access) through the same code path rather than two
// divergent ones.
type remoteControl struct {
	mu      sync.Mutex
	ln      net.Listener
	rtc     *rtcListener // WebRTC clients on the same port number over UDP (rtc.go); nil if it couldn't bind
	stop    context.CancelFunc
	tracker *connTracker

	addr     string
	tlsCert  string
	tlsKey   string
	stateDir string
	deps     connDeps
	settings *ServerSettings
}

// apply copies the persisted remote-access settings into what Start binds and
// presents; it does not touch a listener that is already running.
func (r *remoteControl) apply(cfg RemoteSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addr, r.tlsCert, r.tlsKey = cfg.Addr, cfg.TLSCert, cfg.TLSKey
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
	// Best-effort like the web client: TCP and WebSocket clients don't depend on it.
	if cert, _, err := loadRTCCert(r.stateDir); err != nil {
		logWarn("webrtc: no certificate, WebRTC clients are off: %v", err)
	} else if rl, err := startRTCListener(lctx, addr, cert, func(c net.Conn) {
		tracker.Add(c)
		go func() {
			defer tracker.Remove(c)
			defer c.Close()
			serveAccepted(lctx, c, "tcp", r.deps, r.settings.LookupToken)
		}()
	}); err != nil {
		logWarn("webrtc: couldn't listen on UDP %s, WebRTC clients are off: %v", addr, err)
	} else {
		r.rtc = rl
	}
	printPairQR(ln.Addr(), func() (string, error) {
		tok, err := generateAuthToken()
		if err != nil {
			return "", err
		}
		id, err := generateAuthToken()
		if err != nil {
			return "", err
		}
		// Whoever can read the server's console owns it, so its code grants full access.
		name := "terminal-" + id[:6]
		if err := r.settings.AddPairingToken(name, tok, ""); err != nil {
			return "", err
		}
		r.settings.SetTokenScope(name, true)
		return tok, nil
	}, r.RTCFingerprint(), r.caPEMLocked(), r.deps.pins)
	return nil
}

// RTCFingerprint is the SHA-256 fingerprint of the DTLS certificate WebRTC clients pin, in the
// form an SDP carries (rtc.go). It goes into pairing codes next to the token. "" if the
// certificate can't be made.
func (r *remoteControl) RTCFingerprint() string {
	_, fp, err := loadRTCCert(r.stateDir)
	if err != nil {
		return ""
	}
	return fp
}

// CAPEM returns the auto-managed CA's PEM contents, for bundling into a
// pairing code alongside a freshly registered token (register_token, in
// main.go) — the two travel together since a token is only as trustworthy
// as the connection it's sent over. Returns "" if this server is using an
// operator-supplied certificate instead (resolveServerCert's real-cert
// branch, same -tls-cert/-tls-key test): there's no private CA to hand out
// there, a client just needs the public trust store like normal TLS.
// Cheap enough for every unauthenticated GET /ca.crt: it reads the CA file, and only creates
// the CA when there isn't one yet, so this works even before remote access has ever been turned on.
func (r *remoteControl) CAPEM() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caPEM()
}

// caPEMLocked is CAPEM for a caller already holding r.mu (Start), where a failure just means no
// CA in the code.
func (r *remoteControl) caPEMLocked() string {
	pem, _ := r.caPEM()
	return pem
}

func (r *remoteControl) caPEM() (string, error) {
	if r.tlsCert != "" || r.tlsKey != "" {
		return "", nil
	}
	out, err := tlsca.ReadCAPEM(r.stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		if _, _, _, err := tlsca.EnsureServerCert(r.stateDir, tlsca.DefaultSANs()); err != nil {
			return "", err
		}
		out, err = tlsca.ReadCAPEM(r.stateDir)
	}
	return out, err
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
	if r.rtc != nil {
		r.rtc.Close()
		r.rtc = nil
	}
	_ = r.ln.Close() // don't wait on acceptLoop's own async ctx-cancel watcher to do this
	r.tracker.CloseAll()
	r.ln = nil
	r.tracker = nil
}
