package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sockpath"
	"yoakeland/internal/tlsca"
)

// Server-mode heartbeat tuning: a connection that's gone quiet (no envelope
// of any kind, including a pong) for heartbeatTimeout is presumed dead (e.g.
// a phone that dropped wifi without a clean TCP close) and closed, rather
// than left hanging indefinitely.
const (
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 3 * heartbeatInterval
)

func main() {
	var projectDir string
	var stateDir string
	var providerName string
	var modelName string
	var apiKey string
	var baseURL string
	var listenAddr string
	var authToken string
	var tlsCert string
	var tlsKey string
	var maxSessionCostUSD float64
	var masterKey string
	var stdioMode bool

	flag.StringVar(&projectDir, "project", ".", "Default project root — pre-opened at startup and used as the fallback project for sessions/requests that don't specify their own projectPath. Additional projects can be opened on demand via open_project/user_prompt.")
	flag.StringVar(&stateDir, "state-dir", sockpath.DefaultStateDir(), "Directory for server-level state that isn't tied to any one project: providers.json, and the fallback unix socket path. Defaults to ~/.yoake (Linux), ~/Library/Application Support/yoake (macOS), or %AppData%\\yoake (Windows).")
	flag.StringVar(&providerName, "provider", "auto", "Provider: 'anthropic', 'openai', 'openrouter', or 'auto'")
	flag.StringVar(&modelName, "model", "", "Model name (defaults: claude-3-5-sonnet-20241022, gpt-4o, etc.)")
	flag.StringVar(&apiKey, "api-key", "", "API key (or use ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY)")
	flag.StringVar(&baseURL, "base-url", "", "Custom API base URL (e.g. http://localhost:1234/v1 for LM Studio)")
	flag.BoolVar(&stdioMode, "stdio", false, "Speak the protocol directly over this process's own stdin/stdout and exit when stdin closes, instead of running as a daemon. This is the spawn-a-subprocess-per-client contract (what yoake-client's default -bin mode uses under the hood). Most local usage wants the default instead: a long-running process that always has a local unix socket open, exiting on SIGINT/SIGTERM rather than stdin EOF. Mutually exclusive with -listen.")
	flag.StringVar(&listenAddr, "listen", "", "Optionally ALSO listen here, in addition to the always-on local unix socket (ignored if -stdio is set): a TCP address (e.g. ':7890') for remote/multi-device access, or 'unix:/path/to/socket' to override where the local socket itself lives. A unix socket needs no auth token — the socket file's 0600 permissions (owner-only) are the auth boundary, same as Docker/Postgres/ssh-agent.")
	flag.StringVar(&authToken, "token", "", "Shared auth token required from clients of the optional TCP listener (auto-generated if empty). Not used for any unix socket.")
	flag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file for the optional TCP listener (requires -tls-key). Without both, yoakeland auto-generates and uses its own self-managed CA under -state-dir/tls/ instead — the listener is always TLS, never plaintext.")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS private key file for the optional TCP listener (requires -tls-cert)")
	flag.Float64Var(&maxSessionCostUSD, "max-session-cost-usd", 0, "Refuse to start a new turn once a session has already spent this much (0 = unlimited)")
	flag.StringVar(&masterKey, "master-key", "", "Passphrase encrypting providers.json at rest (AES-256-GCM, scrypt-derived key). Falls back to YOAKELAND_MASTER_KEY if unset. Without either, providers.json is plaintext (0600 perms only).")
	flag.Parse()

	initLogging(stateDir)

	if !stdioMode {
		// stdio mode speaks the protocol over this process's own stdout, so
		// the banner would corrupt the wire format — daemon mode only.
		printBanner()
	}

	if masterKey == "" {
		masterKey = os.Getenv("YOAKELAND_MASTER_KEY")
	}
	if masterKey == "" {
		logWarnDetail(
			"no -master-key set; providers.json will be stored in plaintext",
			"no -master-key/YOAKELAND_MASTER_KEY set — providers.json (API keys) will be stored in PLAINTEXT, protected only by 0600 file permissions.",
		)
	}

	// A default provider is optional, not required to start: a session with
	// no default and no explicit set_provider call is rejected on its first
	// turn (see Agent.RunTurn's nil-provider guard), rather than blocking the
	// whole server from starting before any credentials are registered.
	modelProv, err := provider.ResolveProvider(provider.Config{
		ProviderName: providerName,
		Model:        modelName,
		APIKey:       apiKey,
		BaseURL:      baseURL,
	})
	if err != nil {
		logWarnDetail(
			fmt.Sprintf("no default provider configured: %v", err),
			fmt.Sprintf("no default provider configured (%v) — sessions must register_provider + set_provider before their first user_prompt", err),
		)
	}

	// Projects (sandbox boundary, permissions, snapshots, sessions) are
	// opened on demand and cached here — a single server process can serve
	// many of them at once, each fully isolated from the others. -project is
	// pre-opened eagerly so it fails fast on a bad path, and doubles as the
	// fallback project for anything that doesn't specify its own.
	// providerName here is the -provider flag value ("auto", "anthropic",
	// ...), not a providerStore-registered name — this is the fallback
	// path for a session that never called set_provider, so there's no
	// registered name to stamp; it's still better than leaving every such
	// message's Provider blank.
	projects, err := NewProjectRegistry(modelProv, providerName, maxSessionCostUSD, stateDir)
	if err != nil {
		logFatal("failed to open project list store: %v", err)
	}
	defaultProject, err := projects.Get(projectDir)
	if err != nil {
		logFatal("failed to open default project %q: %v", projectDir, err)
	}

	// Providers/credentials belong to whoever is running the server, not to
	// any one project, so they live in their own state directory rather than
	// inside a project's .yoake/.
	providerStore, err := provider.NewProviderStore(stateDir, masterKey)
	if err != nil {
		logFatal("failed to initialize provider store: %v", err)
	}
	oauthStore := provider.NewOAuthSessionStore()

	// cli_claude/cli_codex's model list comes from T3 Code's model-manifest.json
	// (see internal/provider/manifest.go) rather than a hand-maintained table;
	// the daily background refresh is started further down, once ctx exists.
	manifestCache := provider.NewManifestCache(stateDir)

	// EstimateCostUSD's spend tracking (internal/agent/pricing.go) prices
	// against this live-fetched table (LiteLLM's own model_prices_and_
	// context_window.json — see internal/provider/pricing_cache.go) before
	// falling back to its small hand-maintained one, same reasoning as the
	// manifest cache above: a static map silently prices new/renamed models
	// at $0 with no signal that happened.
	pricingCache := provider.NewPricingCache(stateDir)
	agent.SetLivePricingSource(func(model string) (float64, float64, bool) {
		r, ok := pricingCache.Lookup(model)
		return r.InputPerToken, r.OutputPerToken, ok
	})

	// Settings (server identity, remote-access password) are server-level
	// state like providerStore, persisted under -state-dir so they survive
	// a restart — see settings.go.
	settings, err := NewServerSettings(stateDir)
	if err != nil {
		logFatal("failed to initialize server settings: %v", err)
	}
	if authToken != "" {
		// An explicit -token always wins over whatever was persisted from a
		// previous set_remote_password call — a launch-time flag is an
		// explicit operator override.
		_ = settings.SetToken(authToken)
	}
	peers := NewPeerRegistry()

	rc := &remoteControl{tlsCert: tlsCert, tlsKey: tlsKey, stateDir: stateDir, settings: settings}
	deps := connDeps{
		projects:           projects,
		defaultProjectRoot: defaultProject.Root,
		providerStore:      providerStore,
		oauthStore:         oauthStore,
		manifestCache:      manifestCache,
		peers:              peers,
		settings:           settings,
		remote:             rc,
		turnWG:             &sync.WaitGroup{},
	}
	rc.deps = deps

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	manifestCache.StartDailyRefresh(ctx)
	pricingCache.StartDailyRefresh(ctx)

	if stdioMode {
		serveConnection(ctx, protocol.NewIPCStream(os.Stdin, os.Stdout), deps, "stdio", "", nil)
		// stdin EOF is this mode's clean shutdown signal (the client that owns
		// this process has already gone): give any turn still running the chance
		// to finish its own final save rather than dropping it with the process.
		drainTurns(deps.turnWG)
		return
	}

	// Daemon mode (the default): always open a local unix socket, so any
	// local client can connect immediately with no extra flags — see
	// sockpath and SPEC.md §2a. -listen never replaces this; a TCP address
	// there ADDS an optional, remote-reachable second listener, and a
	// 'unix:<path>' there only overrides where the always-on local socket
	// lives.
	localSocketPath := sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(stateDir))
	var tcpAddr string
	if listenAddr != "" {
		network, address := parseListenAddr(listenAddr, stateDir)
		if network == "unix" {
			localSocketPath = address
		} else {
			tcpAddr = address
		}
	}

	unixLn, err := setupUnixListener(localSocketPath)
	if err != nil {
		logFatal("failed to listen on local socket %s: %v", localSocketPath, err)
	}
	go acceptLoop(ctx, unixLn, "unix", deps, nil, nil)

	if tcpAddr != "" {
		rc.addr = tcpAddr
		if err := rc.Start(ctx); err != nil {
			logFatal("failed to listen on %s: %v", tcpAddr, err)
		}
	}

	<-ctx.Done()
	drainTurns(deps.turnWG)
}

// turnDrainTimeout bounds how long a shutdown waits for in-flight turns.
// ctx is already cancelled by the time this runs, so a turn's model stream and
// any child process are being aborted concurrently and normally finish almost
// immediately; this only exists so one wedged turn can't keep the process
// alive indefinitely.
const turnDrainTimeout = 5 * time.Second

// drainTurns waits (bounded) for every turn goroutine to return, so a turn
// that was running when the server was signalled still reaches its own final
// save. Without this, exiting mid-turn is indistinguishable from losing the
// whole conversation — the prompt included — since the save is the last thing
// a turn does.
func drainTurns(wg *sync.WaitGroup) {
	if wg == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(turnDrainTimeout):
		logWarnDetail(
			fmt.Sprintf("shutting down with turns still running after %s — their sessions may be missing the last turn", turnDrainTimeout),
			fmt.Sprintf("shutdown waited %s for in-flight turns to finish and save; at least one is still running, so its session on disk may be missing the turn in progress. This is expected only if a model request or tool call is ignoring cancellation.", turnDrainTimeout),
		)
	}
}

// setupUnixListener binds a unix domain socket at path, creating its parent
// directory (0700) and clearing any stale socket left by a previous crashed
// run, then chmods the socket file itself 0600 — its owner-only permission
// is the whole auth boundary for connections through it, so no token or
// handshake is needed (see SPEC.md §2a).
func setupUnixListener(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("creating directory for unix socket: %w", err)
	}
	_ = os.Remove(path) // clear a stale socket left by a previous crashed run

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("setting permissions on unix socket: %w", err)
	}
	logInfoDetail(
		fmt.Sprintf("local socket listening at %s", path),
		fmt.Sprintf("yoakeland local socket listening at %s (mode 0600 — no auth token needed, owner-only file permissions are the auth boundary)", path),
	)
	return ln, nil
}

// generateAuthToken produces a fresh random TCP auth token — 16 bytes of
// crypto/rand, hex-encoded — shared by setupTCPListener's auto-generation
// and register_token, so there's exactly one place defining what "a token"
// looks like.
func generateAuthToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// setupTCPListener binds addr for the optional remote listener and
// generates an auth token when one wasn't provided. Returns the token
// actually in effect (generated or passed through) so the caller can gate
// connections against it. Every TCP listener is TLS-wrapped — either an
// operator-supplied cert (-tls-cert/-tls-key) or, by default, a certificate
// from yoakeland's own self-managed CA (see resolveServerCert) — there is no
// plaintext mode any more.
func setupTCPListener(addr, stateDir, tlsCert, tlsKey, authToken string) (net.Listener, string, error) {
	if authToken == "" {
		token, err := generateAuthToken()
		if err != nil {
			return nil, "", fmt.Errorf("generating auth token: %w", err)
		}
		authToken = token
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}

	cert, usingAutoCA, caPEMPath, caFingerprint, err := resolveServerCert(stateDir, tlsCert, tlsKey)
	if err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("resolving TLS certificate: %w", err)
	}
	ln = wrapTLS(ln, cert)

	if usingAutoCA {
		logInfoDetail(
			fmt.Sprintf("remote listener on %s (TLS, self-managed CA, token: %s)", ln.Addr(), authToken),
			fmt.Sprintf("yoakeland remote listener on %s over TLS (self-managed CA, fingerprint %s, CA cert at %s — copy this file to a client for pairing; the client should trust only this CA, not the public trust store) (token: %s)", ln.Addr(), caFingerprint, caPEMPath, authToken),
		)
	} else {
		logInfoDetail(
			fmt.Sprintf("remote listener on %s (TLS, token: %s)", ln.Addr(), authToken),
			fmt.Sprintf("yoakeland remote listener on %s over TLS (token: %s)", ln.Addr(), authToken),
		)
	}
	return ln, authToken, nil
}

// acceptLoop accepts connections on ln until ctx is done, dispatching each
// connTracker records every currently-accepted connection on a listener so
// they can be force-closed together — e.g. remoteControl.Stop() dropping
// every already-connected TCP peer, not just refusing new ones. nil-safe:
// a nil *connTracker (the unix listener's case — those connections are never
// force-dropped as a group) makes Add/Remove/CloseAll no-ops.
type connTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: make(map[net.Conn]struct{})}
}

func (t *connTracker) Add(c net.Conn) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns[c] = struct{}{}
}

func (t *connTracker) Remove(c net.Conn) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, c)
}

// CloseAll force-closes every currently-tracked connection. Each one's own
// accept-loop goroutine notices the close on its next (or already-blocked)
// read and exits normally, removing itself via its own deferred Remove.
func (t *connTracker) CloseAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// through serveConnection on its own goroutine. network selects auth
// behavior: a unix listener accepts an `auth` frame purely to self-report
// peer identity (no token check — its file permissions are already the
// boundary, see setupUnixListener) and tolerates a legacy client that skips
// straight to a normal envelope; anything else requires a valid token from
// tokenGetter (nil for the unix listener, where it's never consulted).
// tracker, if non-nil, records every accepted connection so a caller (e.g.
// remoteControl.Stop()) can force-close them all together — pass nil for a
// listener whose connections are never dropped as a group (the unix socket).
func acceptLoop(ctx context.Context, ln net.Listener, network string, deps connDeps, lookupToken func(token string) (name string, rec TokenRecord, ok bool), tracker *connTracker) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logWarn("accept error on %s: %v", ln.Addr(), err)
			continue
		}

		tracker.Add(conn)

		go func() {
			defer tracker.Remove(conn)
			defer conn.Close()

			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					logWarn("TLS handshake failed from %s: %v", conn.RemoteAddr(), err)
					return
				}
				_ = tlsConn.SetDeadline(time.Time{})
			}

			ipc := protocol.NewIPCStream(conn, conn)
			name, deviceType, avatar, tokenName, email, pending, ok := handshake(ipc, network, lookupToken)
			if !ok {
				return
			}
			if deviceType == "" {
				deviceType = "desktop"
			}

			peer := deps.peers.Add(ipc, conn, network, name, deviceType, avatar)
			if tokenName != "" {
				deps.peers.SetTokenIdentity(peer.ID, tokenName, email)
			}
			defer deps.peers.Remove(peer.ID)

			var lastSeen atomic.Int64
			lastSeen.Store(time.Now().UnixNano())

			hbCtx, stopHeartbeat := context.WithCancel(ctx)
			defer stopHeartbeat()
			go runHeartbeat(hbCtx, ipc, conn, &lastSeen, heartbeatInterval, heartbeatTimeout)
			onActivity := func() { lastSeen.Store(time.Now().UnixNano()) }

			_ = ipc.SendEvent(protocol.EventServerIdentity, "", "", "", protocol.ServerIdentityData{Identity: deps.settings.GetIdentity(), ServerID: deps.settings.GetServerID()})
			_ = ipc.SendEvent(protocol.EventAutoGenerateTitle, "", "", "", protocol.AutoGenerateTitleData{Enabled: deps.settings.GetAutoGenerateTitle()})

			if pending != nil {
				onActivity()
				handleEnvelope(ctx, ipc, deps, network, peer.ID, pending)
			}
			serveConnection(ctx, ipc, deps, network, peer.ID, onActivity)
		}()
	}
}

// runHeartbeat periodically pings an idle connection and closes it if
// nothing at all (including a pong) has been heard from it in
// heartbeatTimeout — catching connections that dropped without a clean TCP
// close (e.g. a phone losing wifi) instead of leaving them open forever.
// ctx cancellation is purely "stop pinging, the connection's lifecycle is
// ending some other way" — it deliberately does NOT close the connection
// itself (callers that need the connection closed on ctx cancellation, e.g.
// remoteControl.Stop(), do that themselves; see acceptLoop's connection
// tracking).
func runHeartbeat(ctx context.Context, ipc *protocol.IPCStream, closer io.Closer, lastSeen *atomic.Int64, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, lastSeen.Load())) > timeout {
				_ = closer.Close()
				return
			}
			if err := ipc.SendEvent(protocol.EventPing, "", "", "", protocol.PingData{}); err != nil {
				_ = closer.Close()
				return
			}
		}
	}
}

// parseListenAddr splits the -listen flag into a net.Listen network/address
// pair: "unix:/path/to/socket" (or bare "unix"/"unix:" to name the default
// path explicitly, see sockpath) overrides where the always-on local socket
// lives; anything else is a TCP address for the optional second listener.
func parseListenAddr(listenAddr, stateDir string) (network, address string) {
	if listenAddr == "unix" {
		return "unix", sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(stateDir))
	}
	if rest, ok := strings.CutPrefix(listenAddr, "unix:"); ok {
		if rest == "" {
			return "unix", sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(stateDir))
		}
		return "unix", rest
	}
	return "tcp", listenAddr
}

// wrapTLS wraps an already-listening net.Listener with an already-resolved
// certificate, so every accepted connection requires a TLS handshake before
// any bytes of the yoakeland protocol are exchanged. Used for both the
// explicit -tls-cert/-tls-key path and the auto-managed CA path (see
// resolveServerCert) — this function doesn't care which one produced cert.
func wrapTLS(ln net.Listener, cert tls.Certificate) net.Listener {
	return tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
}

// resolveServerCert obtains the certificate the TCP listener should present.
// An explicit -tls-cert/-tls-key always wins (an operator-supplied real
// certificate, loaded exactly as before). Otherwise it falls back to
// yoakeland's own self-managed CA (internal/tlsca) — generated once and
// persisted under <-state-dir>/tls/ — rather than running plaintext.
func resolveServerCert(stateDir, tlsCert, tlsKey string) (cert tls.Certificate, usingAutoCA bool, caPEMPath, caFingerprint string, err error) {
	if tlsCert != "" || tlsKey != "" {
		cert, err = tls.LoadX509KeyPair(tlsCert, tlsKey)
		return cert, false, "", "", err
	}
	cert, caPEMPath, caFingerprint, err = tlsca.EnsureServerCert(stateDir, tlsca.DefaultSANs())
	return cert, true, caPEMPath, caFingerprint, err
}

// handshake reads the first frame on a freshly accepted connection.
//
// On a TCP connection it must be an `auth` envelope carrying a token that
// matches tokenGetter() — a mismatch, wrong frame type, or malformed
// payload sends a fatal error and fails the handshake, exactly as before.
//
// On a unix connection (network == "unix") there's no token to check (see
// setupUnixListener), but a client may still choose to send `auth` first
// purely to self-report Name/DeviceType. A legacy/anonymous client that
// skips straight to a normal envelope is equally valid — SPEC.md §2a
// guarantees "no auth frame, no token, at all" for the local socket, and
// this must keep holding — so that first envelope is handed back as pending
// for the caller to dispatch normally, instead of being consumed here.
// constantTimeTokenEqual compares two tokens without leaking timing
// information about where they first differ. subtle.ConstantTimeCompare
// alone isn't enough here since it short-circuits on length mismatch, so
// both sides are first reduced to fixed-size SHA-256 digests.
func constantTimeTokenEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func handshake(ipc *protocol.IPCStream, network string, lookupToken func(token string) (name string, rec TokenRecord, ok bool)) (name, deviceType, avatar, tokenName, email string, pending *protocol.Envelope, ok bool) {
	env, err := ipc.ReadEnvelope()
	if err != nil {
		return "", "", "", "", "", nil, false
	}

	if env.Type != protocol.EventAuth {
		if network == "unix" {
			return "", "", "", "", "", env, true
		}
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "expected auth as first frame",
			Fatal:   true,
		})
		return "", "", "", "", "", nil, false
	}

	var data protocol.AuthData
	if json.Unmarshal(env.Data, &data) != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "malformed auth payload",
			Fatal:   true,
		})
		return "", "", "", "", "", nil, false
	}
	if network == "unix" {
		return data.Name, data.DeviceType, data.Avatar, "", "", nil, true
	}

	resolvedName, rec, found := "", TokenRecord{}, false
	if lookupToken != nil {
		resolvedName, rec, found = lookupToken(data.Token)
	}
	if !found {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "authentication failed",
			Fatal:   true,
		})
		return "", "", "", "", "", nil, false
	}
	// The persisted, token-tied name/avatar always wins over whatever the
	// client self-reported (SPEC.md §2a's per-person tokens) — a name is
	// fixed at register_token time, and the avatar is cached via
	// set_peer_avatar, not re-typed every connection.
	return resolvedName, data.DeviceType, rec.Avatar, resolvedName, rec.Email, nil, true
}

// connDeps bundles the pieces every connection's dispatch loop shares
// (across both stdio mode and every server-mode client). Project-scoped
// state (sandbox, permissions, snapshots, sessions) is not here — it's
// resolved per envelope via projects, since a connection isn't scoped to
// one project any more than the server itself is.
type connDeps struct {
	projects           *ProjectRegistry
	defaultProjectRoot string
	providerStore      *provider.ProviderStore
	oauthStore         *provider.OAuthSessionStore
	manifestCache      *provider.ManifestCache
	peers              *PeerRegistry
	settings           *ServerSettings
	remote             *remoteControl

	// turnWG, if non-nil, is Add(1)'d before every RunTurn goroutine is
	// spawned and Done() when it returns. Production leaves this nil (the
	// dispatch loop is meant to not block on it); tests that need to
	// observe a turn's on-disk side effects after serveConnection returns
	// set it and Wait() on it, instead of racing that goroutine.
	turnWG *sync.WaitGroup
}

// serveConnection dispatches inbound envelopes from a single client
// connection until it disconnects, routing each session's turns to its own
// Agent via the shared registry so concurrent sessions don't interfere.
// network is "unix"/"tcp"/"stdio" (gates the local-socket-only settings
// events); peerID is this connection's entry in deps.peers, or "" for a
// connection that never registered one (currently only -stdio). onActivity,
// if non-nil, is called after every successfully read envelope (server mode
// uses it to feed the heartbeat's liveness check).
func serveConnection(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, onActivity func()) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		env, err := ipc.ReadEnvelope()
		if err != nil {
			if err == io.EOF {
				return
			}
			_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to read envelope: %v", err),
			})
			return
		}
		if onActivity != nil {
			onActivity()
		}
		handleEnvelope(ctx, ipc, deps, network, peerID, env)
	}
}

// sessionWriteError renders a session-store failure for the client, keeping the
// "session not found" wording SPEC.md §3.1 defines for set_session_title and
// friends (rather than leaking the store's wrapped ErrSessionNotFound).
func sessionWriteError(sessionID, verb string, err error) string {
	if errors.Is(err, agent.ErrSessionNotFound) {
		return fmt.Sprintf("session %s not found", sessionID)
	}
	return fmt.Sprintf("failed to %s session: %v", verb, err)
}

// handleEnvelope dispatches one already-read envelope. Split out of
// serveConnection so acceptLoop can run a unix connection's optional pending
// first envelope (see handshake) through the identical dispatch path before
// entering serveConnection's own read loop.
func handleEnvelope(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) {
	switch env.Type {
	case protocol.EventUserPrompt:
		var data protocol.UserPromptData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed user_prompt payload",
			})
			return
		}

		sessID := data.SessionID
		if sessID == "" {
			sessID = env.SessionID
		}
		if sessID == "" {
			sessID = "default"
		}

		pc, err := deps.projects.Resolve(sessID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{
				Message: err.Error(),
			})
			return
		}

		ag := pc.Registry.Get(ipc, sessID)
		// Create the session's file before the turn goroutine starts, so the
		// set_session_title a client pushes alongside a first message (SPEC.md
		// §3.1's title-is-a-client-concern rule) finds a session to rename
		// instead of racing the turn's own first write and being rejected as
		// "not found". The turn appends the prompt to this file a moment later.
		if err := pc.SessStore.Ensure(sessID); err != nil {
			logError("session %s: could not create its session file: %v", sessID, err)
		}
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		// Attribution, in priority order. A token-backed identity is a real
		// credential, so it wins over anything the client says about itself
		// (SPEC.md §2a) and is also what git commits are attributed to. Failing
		// that, a client may declare who it is on the prompt itself
		// (authorName/authorAvatar) — the local unix socket has no credential to
		// attribute to (file permissions are its boundary), so this is the only
		// way a message from it is ever attributed, and without it the transcript
		// has no sender to render. A client that declares nothing (the older
		// behavior, and any third-party client that doesn't care) is stored
		// unattributed rather than rejected.
		var author agent.Author
		if p, ok := deps.peers.Get(peerID); ok && p.TokenName != "" {
			author = agent.Author{Name: p.Name, Email: p.Email}
		} else if data.AuthorName != "" || data.AuthorAvatar != "" {
			avatar := data.AuthorAvatar
			// Cosmetic, so an oversized one is dropped rather than failing the
			// prompt — same rule the peer registry applies to `auth` (§2a). It's
			// stored per message, so a huge data URL would also be duplicated
			// across a whole session file.
			if len(avatar) > maxAvatarLen {
				avatar = ""
			}
			author = agent.Author{Name: data.AuthorName, Avatar: avatar}
		}
		turnCtx := agent.WithAuthor(ctx, author)
		autoGenerateTitle := deps.settings.GetAutoGenerateTitle()
		if data.AutoGenerateTitle != nil {
			autoGenerateTitle = *data.AutoGenerateTitle
		}
		go func(sID, tID, prompt string, autoGenerateTitle bool) {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			if err := ag.RunTurnWithTitleGeneration(turnCtx, sID, tID, prompt, autoGenerateTitle); err != nil {
				logError("turn %s (session %s) failed: %v", tID, sID, err)
				_ = ipc.SendEvent(protocol.EventError, sID, tID, "", protocol.ErrorData{
					Message: err.Error(),
				})
			}
		}(sessID, env.TurnID, data.Prompt, autoGenerateTitle)

	case protocol.EventPermissionResponse:
		var data protocol.PermissionResponseData
		if err := json.Unmarshal(env.Data, &data); err == nil {
			sessID := env.SessionID
			if sessID == "" {
				sessID = "default"
			}
			pc, err := deps.projects.Resolve(sessID, "", deps.defaultProjectRoot)
			if err != nil {
				_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
				return
			}
			pc.Registry.Get(ipc, sessID).HandlePermissionResponse(env.ToolCallID, data.Allowed, data.Remember, data.Feedback)
		}

	case protocol.EventRestoreSnapshot:
		var data protocol.RestoreSnapshotData
		if err := json.Unmarshal(env.Data, &data); err == nil {
			pc, err := deps.projects.Resolve(env.SessionID, data.ProjectPath, deps.defaultProjectRoot)
			if err != nil {
				_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
				return
			}
			if rErr := pc.SnapStore.RestoreSnapshot(data.SnapshotID, pc.Root); rErr != nil {
				_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
					Message: fmt.Sprintf("failed to restore snapshot %s: %v", data.SnapshotID, rErr),
				})
			} else {
				_ = ipc.SendEvent(protocol.EventSnapshotRestored, env.SessionID, env.TurnID, "", protocol.SnapshotEventData{
					SnapshotID: data.SnapshotID,
				})
			}
		}

	case protocol.EventCancelTurn:
		var data protocol.CancelTurnData
		_ = json.Unmarshal(env.Data, &data)
		tID := data.TurnID
		if tID == "" {
			tID = env.TurnID
		}
		sessID := env.SessionID
		if sessID == "" {
			sessID = "default"
		}
		pc, err := deps.projects.Resolve(sessID, "", deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		pc.Registry.Get(ipc, sessID).CancelTurn(tID)

	case protocol.EventRegisterProvider:
		var data protocol.RegisterProviderData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed register_provider payload",
			})
			return
		}
		cfg := provider.ProviderConfig{
			Name:         data.Name,
			Kind:         provider.ProviderKind(data.Kind),
			HTTPProvider: data.HTTPProvider,
			BaseURL:      data.BaseURL,
			APIKey:       data.APIKey,
			AuthMethod:   data.AuthMethod,
		}
		if cfg.AuthMethod == "" {
			cfg.AuthMethod = "manual"
		}
		if err := deps.providerStore.Register(cfg); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to register provider: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventProviderRegistered, env.SessionID, env.TurnID, "", protocol.ProviderRegisteredData{
			Name: cfg.Name,
		})

	case protocol.EventListProviders:
		var summaries []protocol.ProviderSummary
		for _, cfg := range deps.providerStore.List() {
			summaries = append(summaries, protocol.ProviderSummary{
				Name:           cfg.Name,
				Kind:           string(cfg.Kind),
				HTTPProvider:   cfg.HTTPProvider,
				AuthMethod:     cfg.AuthMethod,
				FavoriteModels: cfg.FavoriteModels,
			})
		}
		_ = ipc.SendEvent(protocol.EventProviderList, env.SessionID, env.TurnID, "", protocol.ProviderListData{
			Providers: summaries,
		})

	case protocol.EventListModels:
		var data protocol.ListModelsData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed list_models payload",
			})
			return
		}
		cfg, ok := deps.providerStore.Get(data.ProviderName)
		if !ok {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown provider %q", data.ProviderName),
			})
			return
		}
		models, err := provider.ListModels(cfg, deps.manifestCache)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to list models for %q: %v", data.ProviderName, err),
			})
			return
		}
		favorites := make(map[string]bool, len(cfg.FavoriteModels))
		for _, id := range cfg.FavoriteModels {
			favorites[id] = true
		}
		summaries := make([]protocol.ModelSummary, 0, len(models))
		for _, m := range models {
			summaries = append(summaries, protocol.ModelSummary{ID: m.ID, DisplayName: m.DisplayName, IsFavorite: favorites[m.ID], Capabilities: m.Capabilities, IsLegacy: m.IsLegacy})
		}
		_ = ipc.SendEvent(protocol.EventModelList, env.SessionID, env.TurnID, "", protocol.ModelListData{
			ProviderName: data.ProviderName,
			Models:       summaries,
		})

	case protocol.EventSetFavoriteModels:
		var data protocol.SetFavoriteModelsData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_favorite_models payload",
			})
			return
		}
		if err := deps.providerStore.SetFavoriteModels(data.ProviderName, data.Models); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to set favorite models: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventFavoriteModelsSet, env.SessionID, env.TurnID, "", protocol.FavoriteModelsSetData{
			ProviderName: data.ProviderName,
			Models:       data.Models,
		})

	case protocol.EventSetProvider:
		var data protocol.SetProviderData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_provider payload",
			})
			return
		}
		sessID := env.SessionID
		if sessID == "" {
			sessID = "default"
		}
		// Resolved against data.ProjectPath, not the default-project fallback:
		// a client picks a model for a brand-new chat before that chat's first
		// prompt has bound the session (§4b), and the registry a provider lands
		// in has to be the one the prompt will itself resolve to — otherwise
		// the turn runs on an unconfigured session and comes back no_provider.
		pc, err := deps.projects.Resolve(sessID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		cfg, ok := deps.providerStore.Get(data.ProviderName)
		if !ok {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown provider %q", data.ProviderName),
			})
			return
		}
		built, err := provider.BuildProvider(cfg, data.Model, pc.Root)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to activate provider %q: %v", data.ProviderName, err),
			})
			return
		}
		pc.Registry.Get(ipc, sessID).SetProvider(built, data.ProviderName)
		_ = ipc.SendEvent(protocol.EventProviderSet, sessID, env.TurnID, "", protocol.ProviderSetData{
			SessionID:    sessID,
			ProviderName: data.ProviderName,
			Model:        data.Model,
		})

	case protocol.EventSetModelOptions:
		var data protocol.SetModelOptionsData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_model_options payload",
			})
			return
		}
		sessID := data.SessionID
		if sessID == "" {
			sessID = env.SessionID
		}
		if sessID == "" {
			sessID = "default"
		}
		pc, err := deps.projects.Resolve(sessID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		pc.Registry.Get(ipc, sessID).SetModelOptions(agent.ModelOptions{
			ReasoningEffort: data.ReasoningEffort,
			ThinkingBudget:  data.ThinkingBudget,
		}, data.ContextWindow)
		_ = ipc.SendEvent(protocol.EventModelOptionsSet, sessID, env.TurnID, "", protocol.ModelOptionsSetData{
			SessionID:       sessID,
			ReasoningEffort: data.ReasoningEffort,
			ThinkingBudget:  data.ThinkingBudget,
			ContextWindow:   data.ContextWindow,
		})

	case protocol.EventSetPlanMode:
		var data protocol.SetPlanModeData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_plan_mode payload",
			})
			return
		}
		sessID := data.SessionID
		if sessID == "" {
			sessID = env.SessionID
		}
		if sessID == "" {
			sessID = "default"
		}
		pc, err := deps.projects.Resolve(sessID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		pc.Registry.Get(ipc, sessID).SetPlanMode(data.Enabled)
		_ = ipc.SendEvent(protocol.EventPlanModeSet, sessID, env.TurnID, "", protocol.PlanModeSetData{
			SessionID: sessID,
			Enabled:   data.Enabled,
		})

	case protocol.EventOAuthStart:
		var data protocol.OAuthStartData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed openrouter_oauth_start payload",
			})
			return
		}
		requestID, pkce, err := deps.oauthStore.Begin(data.Name)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to start oauth login: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventOAuthURL, env.SessionID, env.TurnID, "", protocol.OAuthURLData{
			RequestID: requestID,
			URL:       pkce.AuthURL(data.CallbackURL),
		})

	case protocol.EventOAuthComplete:
		var data protocol.OAuthCompleteData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed openrouter_oauth_complete payload",
			})
			return
		}
		pending, ok := deps.oauthStore.Take(data.RequestID)
		if !ok {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "unknown or already-completed oauth request",
			})
			return
		}
		apiKey, err := provider.ExchangeOpenRouterCode(ctx, "", data.Code, pending.Verifier)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("openrouter oauth exchange failed: %v", err),
			})
			return
		}
		cfg := provider.ProviderConfig{
			Name:         pending.Name,
			Kind:         provider.ProviderKindAPIKey,
			HTTPProvider: "openrouter",
			APIKey:       apiKey,
			AuthMethod:   "oauth",
		}
		if err := deps.providerStore.Register(cfg); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to store oauth provider: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventProviderRegistered, env.SessionID, env.TurnID, "", protocol.ProviderRegisteredData{
			Name: cfg.Name,
		})

	case protocol.EventRemoveProvider:
		var data protocol.RemoveProviderData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed remove_provider payload",
			})
			return
		}
		if err := deps.providerStore.Remove(data.Name); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to remove provider: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventProviderRemoved, env.SessionID, env.TurnID, "", protocol.ProviderRemovedData{
			Name: data.Name,
		})

	case protocol.EventListSessions:
		var data protocol.ListSessionsData
		_ = json.Unmarshal(env.Data, &data)

		projectPath := data.ProjectPath
		if projectPath == "" {
			projectPath = deps.defaultProjectRoot
		}
		if projectPath == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     "list_sessions requires a projectPath (no default project is configured)",
				ProjectPath: data.ProjectPath,
			})
			return
		}

		pc, err := deps.projects.Get(projectPath)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to open project %q: %v", projectPath, err),
				ProjectPath: projectPath,
			})
			return
		}

		ids, err := pc.SessStore.List()
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to list sessions: %v", err),
				ProjectPath: projectPath,
			})
			return
		}
		summaries := make([]protocol.SessionSummary, 0, len(ids))
		for _, id := range ids {
			state, err := pc.SessStore.Load(id)
			if err != nil {
				continue
			}
			summaries = append(summaries, sessionSummaryFrom(state))
		}
		_ = ipc.SendEvent(protocol.EventSessionList, env.SessionID, env.TurnID, "", protocol.SessionListData{
			ProjectPath: pc.Root,
			Sessions:    summaries,
		})

	case protocol.EventGetSessionHistory:
		var data protocol.GetSessionHistoryData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed get_session_history payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		state, err := pc.SessStore.Load(data.SessionID)
		var history []agent.Message
		if err == nil {
			history = state.History
		} // a session with no persisted history yet isn't an error — it just has none
		_ = ipc.SendEvent(protocol.EventSessionHistory, data.SessionID, env.TurnID, "", protocol.SessionHistoryData{
			SessionID: data.SessionID,
			Messages:  toHistoryMessages(history),
		})

	case protocol.EventSetSessionTitle:
		var data protocol.SetSessionTitleData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_session_title payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		state, err := pc.SessStore.UpdateExisting(data.SessionID, func(state *agent.SessionState) error {
			state.Title = data.Title
			return nil
		})
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: sessionWriteError(data.SessionID, "save", err)})
			return
		}
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(state))

	case protocol.EventSetSessionArchived:
		var data protocol.SetSessionArchivedData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_session_archived payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		state, err := pc.SessStore.UpdateExisting(data.SessionID, func(state *agent.SessionState) error {
			state.Archived = data.Archived
			if data.Archived {
				now := time.Now().UTC()
				state.ArchivedAt = &now
			} else {
				state.ArchivedAt = nil
			}
			return nil
		})
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: sessionWriteError(data.SessionID, "save", err)})
			return
		}
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(state))

	case protocol.EventSetSessionLayout:
		var data protocol.SetSessionLayoutData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_session_layout payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		state, err := pc.SessStore.UpdateExisting(data.SessionID, func(state *agent.SessionState) error {
			state.LinkedSessionID = data.LinkedSessionID
			return nil
		})
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: sessionWriteError(data.SessionID, "save", err)})
			return
		}
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(state))

	case protocol.EventDeleteSession:
		var data protocol.DeleteSessionData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed delete_session payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		pc.Registry.Remove(data.SessionID)
		if err := pc.SessStore.Delete(data.SessionID); err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to delete session: %v", err),
			})
			return
		}
		deps.projects.Unbind(data.SessionID)
		_ = ipc.SendEvent(protocol.EventSessionDeleted, data.SessionID, env.TurnID, "", protocol.SessionDeletedData{
			SessionID: data.SessionID,
		})

	case protocol.EventListDirectory:
		var data protocol.ListDirectoryData
		_ = json.Unmarshal(env.Data, &data)
		abs, entries, err := listDirectory(data.Path)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to list directory %q: %v", data.Path, err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventDirectoryList, env.SessionID, env.TurnID, "", protocol.DirectoryListData{
			Path:    abs,
			Entries: entries,
		})

	case protocol.EventListSnapshots:
		var data protocol.ListSnapshotsData
		_ = json.Unmarshal(env.Data, &data)

		projectPath := data.ProjectPath
		if projectPath == "" {
			projectPath = deps.defaultProjectRoot
		}
		if projectPath == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     "list_snapshots requires a projectPath (no default project is configured)",
				ProjectPath: data.ProjectPath,
			})
			return
		}

		pc, err := deps.projects.Get(projectPath)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to open project %q: %v", projectPath, err),
				ProjectPath: projectPath,
			})
			return
		}

		snaps, err := pc.SnapStore.ListSnapshots()
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to list snapshots: %v", err),
				ProjectPath: projectPath,
			})
			return
		}
		summaries := make([]protocol.SnapshotSummary, 0, len(snaps))
		for _, snap := range snaps {
			summaries = append(summaries, protocol.SnapshotSummary{
				SnapshotID:  snap.ID,
				Description: snap.Description,
				CreatedAt:   snap.CreatedAt,
			})
		}
		_ = ipc.SendEvent(protocol.EventSnapshotList, env.SessionID, env.TurnID, "", protocol.SnapshotListData{
			ProjectPath: pc.Root,
			Snapshots:   summaries,
		})

	case protocol.EventListFiles:
		var data protocol.ListFilesData
		_ = json.Unmarshal(env.Data, &data)

		projectPath := data.ProjectPath
		if projectPath == "" {
			projectPath = deps.defaultProjectRoot
		}
		if projectPath == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     "list_files requires a projectPath (no default project is configured)",
				ProjectPath: data.ProjectPath,
			})
			return
		}

		pc, err := deps.projects.Get(projectPath)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to open project %q: %v", projectPath, err),
				ProjectPath: projectPath,
			})
			return
		}

		files, err := listProjectFiles(pc.Root)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message:     fmt.Sprintf("failed to list files: %v", err),
				ProjectPath: pc.Root,
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventFileList, env.SessionID, env.TurnID, "", protocol.FileListData{
			ProjectPath: pc.Root,
			Files:       files,
		})

	case protocol.EventOpenProject:
		var data protocol.OpenProjectData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed open_project payload",
			})
			return
		}
		pc, err := deps.projects.Get(data.Path)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to open project %q: %v", data.Path, err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventProjectOpened, env.SessionID, env.TurnID, "", protocol.ProjectOpenedData{
			Path:        pc.Root,
			SandboxTier: pc.SandboxTier,
		})

	case protocol.EventListProjects:
		_ = ipc.SendEvent(protocol.EventProjectList, env.SessionID, env.TurnID, "", protocol.ProjectListData{
			Paths: deps.projects.List(),
		})

	case protocol.EventRemoveProject:
		var data protocol.RemoveProjectData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed remove_project payload",
			})
			return
		}
		key, err := projectKey(data.Path)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to remove project %q: %v", data.Path, err),
			})
			return
		}
		if err := deps.projects.Remove(data.Path); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to remove project %q: %v", data.Path, err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventProjectRemoved, env.SessionID, env.TurnID, "", protocol.ProjectRemovedData{
			Path: key,
		})

	case protocol.EventDetectCLI:
		_ = ipc.SendEvent(protocol.EventCLIAvailability, env.SessionID, env.TurnID, "", protocol.CLIAvailabilityData{
			Claude: provider.DetectCLI(provider.CLIKindClaude),
			Codex:  provider.DetectCLI(provider.CLIKindCodex),
		})

	case protocol.EventPing:
		var data protocol.PingData
		_ = json.Unmarshal(env.Data, &data)
		_ = ipc.SendEvent(protocol.EventPong, env.SessionID, env.TurnID, "", protocol.PongData{Nonce: data.Nonce})

	case protocol.EventPong:
		// No-op: receiving any envelope already refreshed onActivity above;
		// a pong specifically needs no further handling.

	case protocol.EventSetPeerName:
		var data protocol.SetPeerNameData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Name == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_peer_name payload (name is required)",
			})
			return
		}
		if p, ok := deps.peers.Get(peerID); ok && p.TokenName != "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "this connection's identity comes from a registered token and can't be renamed live — remove_token + register_token a new one instead",
			})
			return
		}
		if peerID == "" || !deps.peers.Rename(peerID, data.Name) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "not a registered peer",
			})
		}

	case protocol.EventSetPeerAvatar:
		var data protocol.SetPeerAvatarData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_peer_avatar payload",
			})
			return
		}
		if len(data.Avatar) > maxAvatarLen {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "avatar too large",
			})
			return
		}
		if peerID == "" || !deps.peers.SetAvatar(peerID, data.Avatar) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "not a registered peer",
			})
			return
		}
		// Cache it against the token too (if any) so it survives a
		// reconnect instead of resetting to whatever register_token
		// started with — see TokenRecord.Avatar.
		if p, ok := deps.peers.Get(peerID); ok && p.TokenName != "" {
			deps.settings.SetTokenAvatar(p.TokenName, data.Avatar)
		}

	case protocol.EventDisconnectPeer:
		var data protocol.DisconnectPeerData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.PeerID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed disconnect_peer payload (peerId is required)",
			})
			return
		}
		// Any authenticated peer may disconnect any other: everyone past
		// the handshake already shares the one trust boundary in this
		// version (the unix socket's file perms, or the single shared TCP
		// token) — there's no per-user identity yet to scope this more
		// tightly against, so restricting it further would be security
		// theater rather than a real boundary.
		if !deps.peers.Disconnect(data.PeerID) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown peer %q", data.PeerID),
			})
		}

	case protocol.EventSetServerIdentity:
		var data protocol.SetServerIdentityData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Name == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_server_identity payload (name is required)",
			})
			return
		}
		if err := deps.settings.SetIdentity(data.Name); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to save server identity: %v", err),
			})
			return
		}
		for _, p := range deps.peers.All() {
			_ = p.ipc.SendEvent(protocol.EventServerIdentity, "", "", "", protocol.ServerIdentityData{Identity: data.Name, ServerID: deps.settings.GetServerID()})
		}

	case protocol.EventSetAutoGenerateTitle:
		var data protocol.SetAutoGenerateTitleData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_auto_generate_title payload",
			})
			return
		}
		if err := deps.settings.SetAutoGenerateTitle(data.Enabled); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to save auto-generate-title setting: %v", err),
			})
			return
		}
		for _, p := range deps.peers.All() {
			_ = p.ipc.SendEvent(protocol.EventAutoGenerateTitle, "", "", "", protocol.AutoGenerateTitleData{Enabled: data.Enabled})
		}

	case protocol.EventSetRemoteAccess:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "set_remote_access is local-socket-only",
			})
			return
		}
		var data protocol.SetRemoteAccessData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_remote_access payload",
			})
			return
		}
		var rErr error
		if data.Enabled {
			rErr = deps.remote.Start(ctx)
		} else {
			deps.remote.Stop()
		}
		if rErr != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to enable remote access: %v", rErr),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventRemoteAccessSet, env.SessionID, env.TurnID, "", protocol.RemoteAccessSetData{
			Enabled: data.Enabled,
		})

	case protocol.EventSetRemotePassword:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "set_remote_password is local-socket-only",
			})
			return
		}
		var data protocol.SetRemotePasswordData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Password == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_remote_password payload (password is required)",
			})
			return
		}
		if err := deps.settings.SetToken(data.Password); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to save remote password: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventRemotePasswordSet, env.SessionID, env.TurnID, "", protocol.RemotePasswordSetData{})

	case protocol.EventRegisterToken:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "register_token is local-socket-only",
			})
			return
		}
		var data protocol.RegisterTokenData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Name == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed register_token payload (name is required)",
			})
			return
		}
		token, err := generateAuthToken()
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to generate token: %v", err),
			})
			return
		}
		if err := deps.settings.AddToken(data.Name, token, data.Email); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: err.Error(),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventTokenRegistered, env.SessionID, env.TurnID, "", protocol.TokenRegisteredData{
			Name:  data.Name,
			Token: token,
			Email: data.Email,
		})

	case protocol.EventListTokens:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "list_tokens is local-socket-only",
			})
			return
		}
		summaries := deps.settings.ListTokenNames()
		out := make([]protocol.TokenSummary, len(summaries))
		for i, s := range summaries {
			out[i] = protocol.TokenSummary{Name: s.Name, Email: s.Email, HasAvatar: s.HasAvatar}
		}
		_ = ipc.SendEvent(protocol.EventTokenList, env.SessionID, env.TurnID, "", protocol.TokenListData{Tokens: out})

	case protocol.EventRemoveToken:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "remove_token is local-socket-only",
			})
			return
		}
		var data protocol.RemoveTokenData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Name == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed remove_token payload (name is required)",
			})
			return
		}
		if _, err := deps.settings.RemoveToken(data.Name); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to remove token: %v", err),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventTokenRemoved, env.SessionID, env.TurnID, "", protocol.TokenRemovedData{
			Name: data.Name,
		})
	}
}
