package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
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
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/audit"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/settings"
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

// version is set at release time (-ldflags "-X main.version=0.1.1"); "dev" otherwise.
var version = "dev"

func main() {
	var projectDir string
	var stateDir string
	var providerName string
	var modelName string
	var apiKey string
	var baseURL string
	var listenAddr string
	var tlsCert string
	var tlsKey string
	var maxSessionCostUSD float64
	var masterKey string
	var stdioMode bool
	var webRoot string

	flag.StringVar(&projectDir, "project", "", "Default project root — pre-opened at startup and used as the fallback project for sessions/requests that don't specify their own projectPath. Kept for the -stdio spawn contract (yoake-client -bin \"yoakeland -project X\" sends no projectPath); a daemon's clients pass projectPath and open projects with open_project, so don't set it there.")
	flag.StringVar(&stateDir, "state-dir", sockpath.DefaultStateDir(), "Directory for server-level state that isn't tied to any one project: providers.json, and the fallback unix socket path. Defaults to ~/.yoake (Linux), ~/Library/Application Support/yoake (macOS), or %AppData%\\yoake (Windows).")
	flag.StringVar(&providerName, "provider", "auto", "DEPRECATED, seeds default_provider once (use register_provider + set_default_provider): 'anthropic', 'openai', 'openrouter', or 'auto'")
	flag.StringVar(&modelName, "model", "", "DEPRECATED, seeds default_provider once (use set_default_provider): model name")
	flag.StringVar(&apiKey, "api-key", "", "DEPRECATED, seeds default_provider once (use register_provider): API key. ANTHROPIC_API_KEY, OPENAI_API_KEY and OPENROUTER_API_KEY in the environment still pick a fallback default when none is configured")
	flag.StringVar(&baseURL, "base-url", "", "DEPRECATED, seeds default_provider once (use register_provider): custom API base URL (e.g. http://localhost:1234/v1 for LM Studio)")
	flag.BoolVar(&stdioMode, "stdio", false, "Speak the protocol directly over this process's own stdin/stdout and exit when stdin closes, instead of running as a daemon. This is the spawn-a-subprocess-per-client contract (what yoake-client's default -bin mode uses under the hood). Most local usage wants the default instead: a long-running process that always has a local unix socket open, exiting on SIGINT/SIGTERM rather than stdin EOF. Mutually exclusive with -listen.")
	flag.StringVar(&listenAddr, "listen", "", "'unix:/path/to/socket' overrides where the always-on local socket lives (a named pipe on Windows; ignored if -stdio is set). A unix socket needs no auth token — the socket file's 0600 permissions (owner-only) are the auth boundary, same as Docker/Postgres/ssh-agent. A TCP address (e.g. ':7890') is DEPRECATED: it seeds remote access once; use set_remote_access.")
	flag.StringVar(&tlsCert, "tls-cert", "", "DEPRECATED, seeds remote access once (use set_remote_access {tlsCert, tlsKey}): TLS certificate file for the TCP listener (requires -tls-key). Without both, yoakeland uses its own self-managed CA under -state-dir/tls/ — the listener is always TLS, never plaintext.")
	flag.StringVar(&webRoot, "web-root", "", "DEPRECATED, seeds remote access once (use set_remote_access {webRoot}): directory holding the built web client (the GUI's build/), served to browsers on the remote TCP listener.")
	flag.StringVar(&tlsKey, "tls-key", "", "DEPRECATED, seeds remote access once (use set_remote_access {tlsKey}): TLS private key file (requires -tls-cert)")
	flag.Float64Var(&maxSessionCostUSD, "max-session-cost-usd", 0, "DEPRECATED, seeds maxSessionCostUsd once (use set_runtime_settings): pause a session's turns once it has spent this much (0 = unlimited)")
	flag.StringVar(&masterKey, "master-key", "", "Passphrase encrypting providers.json at rest (AES-256-GCM, scrypt-derived key). Prefer the YOAKELAND_MASTER_KEY environment variable, which this falls back to: it has to be known before providers.json opens, so it can't be a runtime setting. Without either, providers.json is plaintext (0600 perms only).")
	flag.Parse()
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	// State must never land in the working directory, which may be a project.
	if stateDir == "" {
		fmt.Fprintln(os.Stderr, "yoakeland: can't find your home directory for state; pass -state-dir")
		os.Exit(2)
	}
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	if canon, err := normalizer.CanonicalRoot(stateDir); err == nil {
		normalizer.Hidden = []string{canon}
	}

	initLogging(stateDir)

	if !stdioMode {
		// stdio mode speaks the protocol over this process's own stdout, so
		// the banner would corrupt the wire format — daemon mode only.
		printBanner()
	}

	if masterKey == "" {
		masterKey = os.Getenv("YOAKELAND_MASTER_KEY")
	}
	// CLI agents (native_proc.go) inherit this process's environment.
	_ = os.Unsetenv("YOAKELAND_MASTER_KEY")
	warnSecretFlags()
	if masterKey == "" {
		logWarnDetail(
			"no -master-key set; providers.json will be stored in plaintext",
			"no -master-key/YOAKELAND_MASTER_KEY set — providers.json (API keys) will be stored in PLAINTEXT, protected only by 0600 file permissions.",
		)
	}

	// A default provider is optional, not required to start: a session with
	// no default and no explicit set_provider call is rejected on its first
	// turn (see Agent.RunTurn's nil-provider guard), rather than blocking the
	// whole server from starting before any credentials are registered. This
	// one is only the fallback, built from the environment (and, for one more
	// release, the deprecated flags): the default_provider setting, once one is
	// stored, takes precedence over it (defaultProviderSource).
	modelProv, err := provider.ResolveProvider(provider.Config{
		ProviderName: providerName,
		Model:        modelName,
		APIKey:       apiKey,
		BaseURL:      baseURL,
	})
	if err != nil {
		logInfoDetail(
			"no environment default provider; use set_default_provider or set_provider per session",
			fmt.Sprintf("no provider could be picked from the environment (%v) — register one with register_provider, then set_default_provider (or set_provider per session) before the first user_prompt", err),
		)
	}

	// Projects (sandbox boundary, permissions, snapshots, sessions) are
	// opened on demand and cached here — a single server process can serve
	// many of them at once, each fully isolated from the others. If -project
	// is given, it's pre-opened eagerly so it fails fast on a bad path, and
	// doubles as the fallback project for anything that doesn't specify its
	// own. Left unset (the default), no project is opened at startup and no
	// implicit fallback exists — a GUI client that never passes -project
	// (yoake-gui doesn't; projects are opened on demand over the wire, §4b)
	// must not have its own launch cwd silently adopted and persisted into
	// projects.json as a "known project".
	// providerName here is the -provider flag value ("auto", "anthropic",
	// ...), not a providerStore-registered name — this is the fallback
	// path for a session that never called set_provider, so there's no
	// registered name to stamp; it's still better than leaving every such
	// message's Provider blank.
	projects, err := NewProjectRegistry(modelProv, providerName, maxSessionCostUSD, stateDir)
	if err != nil {
		logFatal("failed to open project list store: %v", err)
	}
	// Before any project opens: each one's browser tool relays through it.
	peers := NewPeerRegistry()
	browser := newBrowserBroker(peers)
	projects.browser = browser
	// Providers/credentials belong to whoever is running the server, not to
	// any one project.
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
	agent.SetLivePricingSource(func(model string, u protocol.UsageData) (float64, bool) {
		return pricingCache.CostUSD(model, u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, u.OutputTokens)
	})
	// The same table's max_input_tokens is the last stop for a model's
	// context window, after OpenRouter's own context_length.
	agent.SetContextWindowLookup(func(model string) int {
		if n := provider.OpenRouterContextLength(model); n > 0 {
			return n
		}
		return pricingCache.ContextWindow(model)
	})

	// Settings (server identity, remote-access password) are server-level
	// state like providerStore, persisted under -state-dir so they survive
	// a restart — see settings.go.
	settings, err := NewServerSettings(stateDir)
	if err != nil {
		logFatal("failed to initialize server settings: %v", err)
	}
	reloadRoles(projects.roles, providerStore, settings)

	// Deprecated launch flags hand their value over once, then the stored
	// setting rules (runtime_dispatch.go).
	tcpAddr, localSocketPath := "", ""
	if listenAddr != "" && !stdioMode {
		network, address := parseListenAddr(listenAddr, stateDir)
		if network == "unix" {
			localSocketPath = address
		} else {
			tcpAddr = address
		}
	}
	seedFromFlags(launchFlags{
		provider: providerName, model: modelName, apiKey: apiKey, baseURL: baseURL,
		maxSessionCostUSD: maxSessionCostUSD, tcpAddr: tcpAddr,
		tlsCert: tlsCert, tlsKey: tlsKey, webRoot: webRoot,
	}, setFlags, settings, providerStore)

	// The runtime settings' consumers: attached before any project opens, so
	// each one is born with the current values.
	settings.OnRuntimeChange(projects.ApplyRuntime)
	settings.OnRuntimeChange(applyProcessLimits(pricingCache))
	notify := OpenNotificationFeed(peers, filepath.Join(stateDir, "notifications.jsonl"), settings.GetNotifyPrefs)
	projects.SetNotifications(notify)
	// The audit trail (internal/audit): a failure to open it is loud but not
	// fatal, and leaves a nil log, which records nothing.
	auditLog, err := audit.Open(filepath.Join(stateDir, "audit"))
	if err != nil {
		logWarn("audit log unavailable, security events will not be recorded: %v", err)
	} else {
		auditLog.Warn = logWarn
	}
	projects.SetAudit(auditLog)
	hub := newStatusHub(peers, stateDir)
	projects.SetStatusHub(hub)
	projects.SetDefaultSource(defaultProviderSource(settings, providerStore, hub))
	uploads, err := NewUploadStore(stateDir)
	if err != nil {
		logFatal("failed to initialize upload store: %v", err)
	}

	// Pre-opened (failing fast on a bad path) only after everything a
	// project's sessions read at creation exists.
	var defaultProjectRoot string
	if projectDir != "" {
		defaultProject, err := projects.Get(projectDir)
		if err != nil {
			logFatal("failed to open default project %q: %v", projectDir, err)
		}
		defaultProjectRoot = defaultProject.Root
	}

	rc := &remoteControl{stateDir: stateDir, settings: settings}
	rc.apply(settings.Remote())
	deps := connDeps{
		projects:           projects,
		defaultProjectRoot: defaultProjectRoot,
		providerStore:      providerStore,
		pricing:            pricingCache,
		oauthStore:         oauthStore,
		manifestCache:      manifestCache,
		peers:              peers,
		notify:             notify,
		audit:              auditLog,
		presence:           newPresenceNotes(notify),
		pairings:           newPairingGate(),
		uploads:            uploads,
		settings:           settings,
		remote:             rc,
		status:             hub,
		browser:            browser,
		turnWG:             &sync.WaitGroup{},
	}
	rc.deps = deps
	hub.status = func() *protocol.ServerStatus { return serverStatus(deps) }

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	manifestCache.StartDailyRefresh(ctx)
	pricingCache.StartDailyRefresh(ctx)
	hub.StartUsagePolling(ctx, providerStore)

	if stdioMode {
		serveConnection(ctx, protocol.NewIPCStream(os.Stdin, os.Stdout), deps, "stdio", "", nil)
		// stdin EOF is this mode's clean shutdown signal (the client that owns
		// this process has already gone): give any turn still running the chance
		// to finish its own final save rather than dropping it with the process.
		drainTurns(deps.turnWG)
		deps.projects.CloseMCP()
		return
	}

	// Daemon mode (the default): always open a local unix socket, so any
	// local client can connect immediately with no extra flags — see
	// sockpath and SPEC.md §2a. Remote access never replaces this: the TCP
	// listener (set_remote_access) ADDS an optional, remote-reachable second
	// one, and a -listen 'unix:<path>' only overrides where the always-on
	// local socket lives.
	defaultSocketPath := sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(stateDir))
	if localSocketPath == "" {
		localSocketPath = defaultSocketPath
	}

	localLn, err := setupLocalListener(localSocketPath, localSocketPath == defaultSocketPath)
	if err != nil {
		logFatal("failed to listen on local socket %s: %v", localSocketPath, err)
	}
	// "unix" network semantics (no token) for the Windows named pipe too:
	// its owner-only ACL is the same boundary the socket file's 0600 is.
	go acceptLoop(ctx, localLn, "unix", deps, nil, nil)

	// Remote access comes back on its own if it was left on (set_remote_access
	// persists it). A listener that cannot bind is reported, not fatal: the
	// local socket is what a client needs to turn it off or move it.
	if settings.Remote().Enabled {
		if err := rc.Start(ctx); err != nil {
			logWarn("remote access is enabled but the listener failed to start: %v", err)
		}
	}

	<-ctx.Done()
	drainTurns(deps.turnWG)
	deps.projects.CloseMCP() // the MCP servers' processes don't outlive the daemon
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
	// The CLI agents' processes are kept alive between turns; none outlives the server.
	defer provider.CloseAllNativeSessions()
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

// minRemotePasswordLen: a chosen password guards the same full access a
// generated 32-hex-char token does, with no lockout on wrong guesses.
const minRemotePasswordLen = 16

// secretFlags name the environment variable to use instead of each flag
// that carries a secret: a process's argv is readable by every local user
// (/proc/<pid>/cmdline, ps), its environment only by the same user.
var secretFlags = map[string]string{
	"master-key": "YOAKELAND_MASTER_KEY",
	"api-key":    "ANTHROPIC_API_KEY / OPENAI_API_KEY / OPENROUTER_API_KEY",
}

func warnSecretFlags() {
	flag.Visit(func(f *flag.Flag) {
		if env, ok := secretFlags[f.Name]; ok {
			logWarnDetail(
				fmt.Sprintf("-%s on the command line is visible to other local users; use %s", f.Name, env),
				fmt.Sprintf("-%s was passed on the command line, where any local user can read it (ps, /proc/<pid>/cmdline). Set %s in the environment instead.", f.Name, env),
			)
		}
	})
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

// pairingCode bundles a token with a CA certificate (omitted if empty, the
// operator-supplied-real-cert case) into one opaque, copy-pasteable string —
// the exact {"t","ca"} JSON-then-base64 shape src/lib/pairing.ts decodes
// client-side, so a code printed here pastes directly into a client's "Add a
// server" pairing-code field: no separate CA file to hand over, whoever's
// pairing just reads this one line off the server's own console. rtcFp (the
// DTLS fingerprint, rtc.go) lets a browser connect over WebRTC instead.
func pairingCode(token, caPem, rtcFp string) string {
	payload := struct {
		Token string `json:"t"`
		CA    string `json:"ca,omitempty"`
		FP    string `json:"fp,omitempty"`
	}{Token: token, CA: caPem, FP: rtcFp}
	b, _ := json.Marshal(payload) // an all-string struct — Marshal can't fail
	return base64.StdEncoding.EncodeToString(b)
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

	cert, usingAutoCA, caPEMPath, _, err := resolveServerCert(stateDir, tlsCert, tlsKey)
	if err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("resolving TLS certificate: %w", err)
	}
	ln = wrapTLS(ln, cert)

	var caPEM string
	if usingAutoCA {
		// Best-effort: worst case the printed code just omits the CA (same as
		// the operator-real-cert branch below), not a listener failure.
		if b, err := os.ReadFile(caPEMPath); err == nil {
			caPEM = string(b)
		}
	}
	carries := "the token"
	if usingAutoCA {
		carries = "the token and this server's CA"
	}
	var rtcFp string
	if _, fp, err := loadRTCCert(stateDir); err == nil {
		rtcFp = fp
	}
	code := pairingCode(authToken, caPEM, rtcFp)
	logInfoDetail(
		fmt.Sprintf("remote listener on %s (TLS) — pairing code: %s", ln.Addr(), code),
		fmt.Sprintf("yoakeland remote listener on %s over TLS — paste this pairing code into a client's \"Add a server\" (it carries %s): %s", ln.Addr(), carries, code),
	)
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

			var c net.Conn = conn
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					logWarn("TLS handshake failed from %s: %v", conn.RemoteAddr(), err)
					return
				}
				// Clients open with a JSON auth frame; anything else is a browser.
				br := bufio.NewReader(conn)
				first, err := br.Peek(1)
				_ = tlsConn.SetDeadline(time.Time{})
				if err != nil {
					return
				}
				c = &bufferedConn{Conn: conn, r: br}
				if first[0] != '{' {
					serveWeb(ctx, c, network, deps, lookupToken)
					return
				}
			}
			serveAccepted(ctx, c, network, deps, lookupToken)
		}()
	}
}

// Until a TCP connection authenticates it's bounded the way OpenSSH bounds
// one (LoginGraceTime, MaxStartups): preAuthTimeout to send its auth frame,
// at most maxPreAuth such connections at once (the rest are dropped), and a
// failed attempt holds its slot for authFailDelay — so a stranger can't pile
// up idle or 10MB-frame connections, or guess passwords at line rate.
const (
	maxPreAuth    = 32
	authFailDelay = time.Second
)

var preAuthTimeout = 10 * time.Second // var for tests

var preAuthSlots = make(chan struct{}, maxPreAuth)

// Shared by socket clients and browser WebSockets (web.go).
func serveAccepted(ctx context.Context, conn net.Conn, network string, deps connDeps, lookupToken func(token string) (name string, rec TokenRecord, ok bool)) {
	ipc := protocol.NewIPCStream(conn, conn)
	if network == "tcp" {
		select {
		case preAuthSlots <- struct{}{}:
		default:
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(preAuthTimeout))
	}
	// A stranger presenting a wrong credential is what the audit log wants;
	// one that just connects and goes away is not.
	badCredential := false
	var tokenRec TokenRecord
	if lookupToken != nil {
		inner := lookupToken
		lookupToken = func(token string) (string, TokenRecord, bool) {
			name, rec, found := inner(token)
			badCredential = badCredential || !found
			tokenRec = rec
			return name, rec, found
		}
	}
	who, pending, ok := handshake(ipc, network, lookupToken)
	if network == "tcp" {
		if !ok {
			time.Sleep(authFailDelay)
		}
		<-preAuthSlots
		_ = conn.SetReadDeadline(time.Time{})
	}
	if !ok {
		if badCredential {
			deps.recordAuthFail(remoteIP(conn, network), network)
		}
		return
	}
	if who.DeviceType == "" {
		who.DeviceType = "desktop"
	}
	if who.TokenName != "" {
		if err := deps.admitPairing(ipc, who, tokenRec, remoteIP(conn, network)); err != nil {
			_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: err.Error(), Fatal: true})
			return
		}
	}

	peer := deps.peers.Add(ipc, conn, network, who)
	defer deps.peers.Remove(peer.ID)
	deps.recordPeer(audit.PeerJoin, peer)
	defer deps.recordPeer(audit.PeerLeave, peer)
	defer deps.uploads.DropPeer(peer.ID)
	defer deps.browser.DropPeer(peer.ID)
	// The poller idles with nobody connected; catch up for this client.
	deps.status.KickUsage()
	if network == "tcp" {
		// Remote devices only: a local GUI reconnecting is routine noise.
		deps.presence.Joined(peer)
		defer deps.presence.Left(peer)
	}

	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	status := func() *protocol.ServerStatus { return serverStatus(deps) }
	go runHeartbeat(hbCtx, ipc, conn, &lastSeen, heartbeatInterval, heartbeatTimeout, status)
	onActivity := func() { lastSeen.Store(time.Now().UnixNano()) }

	_ = ipc.SendEvent(protocol.EventServerIdentity, "", "", "", protocol.ServerIdentityData{Identity: deps.settings.GetIdentity(), ServerID: deps.settings.GetServerID(), Version: version})
	_ = ipc.SendEvent(protocol.EventAutoGenerateTitle, "", "", "", protocol.AutoGenerateTitleData{Enabled: deps.settings.GetAutoGenerateTitle()})
	_ = ipc.SendEvent(protocol.EventModelRolesSet, "", "", "", protocol.ModelRolesSetData{Roles: deps.settings.GetModelRoles()})
	_ = ipc.SendEvent(protocol.EventNotifyPrefs, "", "", "", deps.settings.GetNotifyPrefs())
	sendRuntimeState(ipc, deps, network)
	deps.status.SendPendingAsks(ipc)
	for _, st := range deps.projects.GitStatuses() {
		_ = ipc.SendEvent(protocol.EventGitStatus, "", "", "", st)
	}
	deps.status.SendTerminals(ipc)

	if pending != nil {
		onActivity()
		handleEnvelope(ctx, ipc, deps, network, peer.ID, pending)
	}
	serveConnection(ctx, ipc, deps, network, peer.ID, onActivity)
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
// status, if non-nil, is sampled into every ping (SPEC.md §2a) — and once
// immediately, so a fresh connection's status bar isn't blank for a whole
// interval.
func runHeartbeat(ctx context.Context, ipc *protocol.IPCStream, closer io.Closer, lastSeen *atomic.Int64, interval, timeout time.Duration, status func() *protocol.ServerStatus) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ping := func() error {
		data := protocol.PingData{}
		if status != nil {
			data.Status = status()
		}
		return ipc.SendEvent(protocol.EventPing, "", "", "", data)
	}
	if status != nil {
		_ = ping()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, lastSeen.Load())) > timeout {
				_ = closer.Close()
				return
			}
			if err := ping(); err != nil {
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

func handshake(ipc *protocol.IPCStream, network string, lookupToken func(token string) (name string, rec TokenRecord, ok bool)) (who peerIdentity, pending *protocol.Envelope, ok bool) {
	env, err := ipc.ReadEnvelope()
	if err != nil {
		return who, nil, false
	}

	if env.Type != protocol.EventAuth {
		if network == "unix" {
			return who, env, true
		}
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "expected auth as first frame",
			Fatal:   true,
		})
		return who, nil, false
	}

	var data protocol.AuthData
	if json.Unmarshal(env.Data, &data) != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: "malformed auth payload",
			Fatal:   true,
		})
		return who, nil, false
	}
	who = peerIdentity{Name: data.Name, DeviceType: data.DeviceType, Avatar: data.Avatar, DeviceID: data.DeviceID}
	if network == "unix" {
		return who, nil, true
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
		return peerIdentity{}, nil, false
	}
	// The token's name is only the initial name for a device that has none yet (it is what the
	// person typed when pairing it); a device that already knows who it is keeps that identity.
	// The credential's own name stays in TokenName for revocation and the audit log.
	who.TokenName, who.Email = resolvedName, rec.Email
	if who.Name == "" {
		who.Name = resolvedName
	}
	if who.Avatar == "" {
		who.Avatar = rec.Avatar
	}
	return who, nil, true
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
	pricing            *provider.PricingCache
	oauthStore         *provider.OAuthSessionStore
	manifestCache      *provider.ManifestCache
	peers              *PeerRegistry
	notify             *NotificationFeed
	audit              *audit.Log
	presence           *presenceNotes
	pairings           *pairingGate
	uploads            *UploadStore
	settings           *ServerSettings
	remote             *remoteControl
	status             *statusHub
	browser            *browserBroker
	webRoot            string // "" serves no web UI

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
		dispatchEnvelope(ctx, ipc, deps, network, peerID, env)
	}
}

// dispatchEnvelope runs handleEnvelope with panic recovery: every other
// request/response event (set_session_title, delete_session, ...) is
// handled synchronously right here, on this connection's own goroutine, so
// an unrecovered panic anywhere in there would previously take the whole
// process down — every other session's connection with it — same failure
// mode recoverTurnPanic closes for the async turn goroutines, and reused
// here rather than duplicated.
func dispatchEnvelope(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) {
	defer recoverTurnPanic(ipc, env.SessionID, env.TurnID)
	handleEnvelope(ctx, ipc, deps, network, peerID, env)
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

// resolveAuthor determines who to attribute a new user message (and any git
// commits its tool calls make) to. A connection signed in with a token shows
// the name and picture its device reports (falling back to the token's name),
// but commits are signed with the credential's name and email, which the
// device doesn't choose. Failing that, a client may self-report via
// authorName/authorAvatar — the local unix socket has no credential to
// attribute to (file permissions are its boundary), so this is the only way
// a message from it is ever attributed, and without it the transcript has no
// sender to render. A client (or event, like retry_message, which carries
// neither field) that declares nothing is stored unattributed rather than
// rejected. Shared by user_prompt, edit_message, and retry_message.
func resolveAuthor(deps connDeps, peerID, authorName, authorAvatar string) agent.Author {
	if name, avatar, credential, email, ok := deps.peers.Author(peerID); ok && credential != "" {
		return agent.Author{Name: name, Avatar: avatar, Email: email, Credential: credential}
	}
	if authorName != "" || authorAvatar != "" {
		avatar := authorAvatar
		// Cosmetic, so an oversized one is dropped rather than failing the
		// prompt — same rule the peer registry applies to `auth` (§2a). It's
		// stored per message, so a huge data URL would also be duplicated
		// across a whole session file.
		if len(avatar) > maxAvatarLen {
			avatar = ""
		}
		return agent.Author{Name: authorName, Avatar: avatar}
	}
	return agent.Author{}
}

// recoverTurnPanic catches a panic from a turn goroutine (user_prompt/
// edit_message/retry_message all run one) and reports it as an ordinary
// turn error instead of letting it crash the whole process — every other
// session on this server dies with it otherwise, which is what turned a
// single bad turn into "chat deletion sometimes fails, restart fixes it":
// there was no server left to answer the delete. Deferred first thing in
// each such goroutine, after any turnWG.Done() defer so that still runs.
func recoverTurnPanic(ipc *protocol.IPCStream, sessionID, turnID string) {
	if r := recover(); r != nil {
		logError("turn %s (session %s) panicked: %v\n%s", turnID, sessionID, r, debug.Stack())
		_ = ipc.SendEvent(protocol.EventError, sessionID, turnID, "", protocol.ErrorData{
			Message: fmt.Sprintf("internal error: %v", r),
		})
	}
}

// handleEnvelope dispatches one already-read envelope. Split out of
// serveConnection so acceptLoop can run a unix connection's optional pending
// first envelope (see handshake) through the identical dispatch path before
// entering serveConnection's own read loop.
// resolveTerminalSession resolves the (project, sessionId) a terminal_*/
// list_terminals envelope targets — terminals are bound to a chat session
// (deleting the chat kills its terminals, see ProjectContext.Terminals'
// CloseSession callers), so this mirrors user_prompt's own resolution
// exactly: an explicit sessionId, falling back to the envelope's own
// sessionId, then resolved against ProjectRegistry the same way
// deps.projects.Resolve handles every other session-scoped event. On
// failure it sends the error envelope itself and returns a non-nil error so
// the caller can just `return`.
func resolveTerminalSession(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope, sessionID, projectPath string) (*ProjectContext, string, error) {
	sessID := sessionID
	if sessID == "" {
		sessID = env.SessionID
	}
	if sessID == "" {
		err := fmt.Errorf("terminal requires a sessionId")
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
			Message: err.Error(),
		})
		return nil, "", err
	}
	pc, err := deps.projects.Resolve(sessID, projectPath, deps.defaultProjectRoot)
	if err != nil {
		_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
		return nil, "", err
	}
	return pc, sessID, nil
}

func handleEnvelope(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) {
	if network == "tcp" {
		if err := scopeRemoteProject(deps, env); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
	}
	if handleFileEnvelope(ipc, deps, env) {
		return
	}
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

		if err := validateAttachments(network, deps.uploads, pc.Root, data.Attachments); err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
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
		author := resolveAuthor(deps, peerID, data.AuthorName, data.AuthorAvatar)
		turnCtx := agent.WithAttachments(agent.WithAuthor(ctx, author), data.Attachments)
		autoGenerateTitle := deps.settings.GetAutoGenerateTitle()
		if data.AutoGenerateTitle != nil {
			autoGenerateTitle = *data.AutoGenerateTitle
		}
		go func(sID, tID, prompt string, autoGenerateTitle bool) {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer recoverTurnPanic(ipc, sID, tID)
			if err := ag.RunTurnWithTitleGeneration(turnCtx, sID, tID, prompt, autoGenerateTitle); err != nil {
				logError("turn %s (session %s) failed: %v", tID, sID, err)
				deps.notify.turnFailed(pc, sID, err)
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
			pc.Registry.Get(ipc, sessID).HandlePermissionResponse(env.ToolCallID, data.Allowed, data.Remember, data.Feedback, agent.PermissionChoice{RuleID: data.RuleID, Scope: data.Scope, By: deps.actorName(ipc, peerID)})
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
				deps.record(ipc, peerID, audit.Event{Kind: audit.SnapshotRestore, Project: pc.Root, Session: env.SessionID, Target: data.SnapshotID, Outcome: "error", Detail: map[string]any{"error": rErr.Error()}})
				_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
					Message: fmt.Sprintf("failed to restore snapshot %s: %v", data.SnapshotID, rErr),
				})
			} else {
				deps.record(ipc, peerID, audit.Event{Kind: audit.SnapshotRestore, Project: pc.Root, Session: env.SessionID, Target: data.SnapshotID, Outcome: "ok"})
				restored := protocol.SnapshotEventData{SnapshotID: data.SnapshotID}
				_ = ipc.SendEvent(protocol.EventSnapshotRestored, env.SessionID, env.TurnID, "", restored)
				// This reverted real files on disk — every other peer with
				// this session open needs to know its file view just went
				// stale, not just the connection that asked for the revert.
				deps.peers.BroadcastEvent(peerID, protocol.EventSnapshotRestored, env.SessionID, restored)
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
		ag := pc.Registry.Get(ipc, sessID)
		if data.QueuedOnly {
			ag.WithdrawTurn(tID)
		} else {
			ag.CancelTurn(tID)
		}

	case protocol.EventSteerTurn:
		var data protocol.SteerTurnData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: "malformed steer_turn payload"})
			return
		}
		sessID := data.SessionID
		if sessID == "" {
			sessID = env.SessionID
		}
		if sessID == "" {
			sessID = "default"
		}
		turnID := data.TurnID
		if turnID == "" {
			turnID = env.TurnID
		}
		pc, err := deps.projects.Resolve(sessID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, sessID, turnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		// Accepted, the running turn records the message and says steer_accepted; refused,
		// the client sends it as a prompt of its own, which waits its turn.
		author := resolveAuthor(deps, peerID, data.AuthorName, data.AuthorAvatar)
		if err := pc.Registry.Get(ipc, sessID).Steer(turnID, data.Prompt, author); err != nil {
			_ = ipc.SendEvent(protocol.EventSteerRejected, sessID, turnID, "", protocol.SteerResultData{Prompt: data.Prompt, Reason: err.Error()})
		}

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
		// Never the key: what was registered, and where it points.
		deps.record(ipc, peerID, audit.Event{Kind: audit.ProviderAdd, Target: cfg.Name, Outcome: "ok", Detail: map[string]any{
			"providerKind": string(cfg.Kind), "httpProvider": cfg.HTTPProvider, "baseUrl": cfg.BaseURL, "authMethod": cfg.AuthMethod,
		}})
		registered := protocol.ProviderRegisteredData{Name: cfg.Name}
		_ = ipc.SendEvent(protocol.EventProviderRegistered, env.SessionID, env.TurnID, "", registered)
		// Server-wide state (§6b/§6c) — every peer's provider list needs it,
		// not just this connection's.
		deps.peers.BroadcastEvent(peerID, protocol.EventProviderRegistered, "", registered)
		if _, cli := provider.CLIKindOf(cfg.Kind); cli {
			deps.status.KickUsage()
		}
		// A role on this provider still holds the old credentials.
		reloadRoles(deps.projects.roles, deps.providerStore, deps.settings)

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
		// Discovery spawns the CLI or dials the endpoint, tens of seconds when
		// that host is down; inline it would stall every other request on this
		// connection (list_directory, get_session_history, ...) behind it.
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func() {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer recoverTurnPanic(ipc, env.SessionID, env.TurnID)
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
		}()

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
		favoritesSet := protocol.FavoriteModelsSetData{
			ProviderName: data.ProviderName,
			Models:       data.Models,
		}
		_ = ipc.SendEvent(protocol.EventFavoriteModelsSet, env.SessionID, env.TurnID, "", favoritesSet)
		deps.peers.BroadcastEvent(peerID, protocol.EventFavoriteModelsSet, "", favoritesSet)

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
		if cli, ok := built.(*provider.CLISubprocessProvider); ok && deps.status != nil {
			name := data.ProviderName
			kind := string(cfg.Kind)
			cli.OnRateLimits = func(rl protocol.ProviderRateLimits) {
				rl.Kind = kind
				deps.status.SetRateLimits(name, rl)
			}
		}
		pc.Registry.Get(ipc, sessID).SetProvider(built, data.ProviderName)
		providerSet := protocol.ProviderSetData{
			SessionID:    sessID,
			ProviderName: data.ProviderName,
			Model:        data.Model,
		}
		_ = ipc.SendEvent(protocol.EventProviderSet, sessID, env.TurnID, "", providerSet)
		deps.peers.BroadcastEvent(peerID, protocol.EventProviderSet, sessID, providerSet)

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
			FastMode:        data.FastMode,
		}, data.ContextWindow, data.AutoCompactTokens, data.AutoCompact)
		modelOptsSet := protocol.ModelOptionsSetData{
			SessionID:         sessID,
			ReasoningEffort:   data.ReasoningEffort,
			ThinkingBudget:    data.ThinkingBudget,
			ContextWindow:     data.ContextWindow,
			FastMode:          data.FastMode,
			AutoCompactTokens: data.AutoCompactTokens,
			AutoCompact:       data.AutoCompact,
		}
		_ = ipc.SendEvent(protocol.EventModelOptionsSet, sessID, env.TurnID, "", modelOptsSet)
		deps.peers.BroadcastEvent(peerID, protocol.EventModelOptionsSet, sessID, modelOptsSet)

	case protocol.EventSetModelRoles:
		var data protocol.SetModelRolesData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_model_roles payload",
			})
			return
		}
		live, err := buildRoles(deps.providerStore, data.Roles)
		if err == nil {
			err = deps.settings.SetModelRoles(data.Roles)
		}
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		deps.projects.roles.Set(live)
		rolesSet := protocol.ModelRolesSetData{Roles: data.Roles}
		_ = ipc.SendEvent(protocol.EventModelRolesSet, "", env.TurnID, "", rolesSet)
		deps.peers.BroadcastEvent(peerID, protocol.EventModelRolesSet, "", rolesSet)

	case protocol.EventSetMode:
		var data protocol.SetModeData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_mode payload",
			})
			return
		}
		mode := agent.Mode(data.Mode)
		if !agent.ValidMode(data.Mode) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown mode %q", data.Mode),
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
		ag := pc.Registry.Get(ipc, sessID)
		prevMode := ag.Mode()
		ag.SetMode(mode)
		if prevMode != mode { // clients re-assert their cached mode on reconnect: only a change is news
			deps.record(ipc, peerID, audit.Event{Kind: audit.ModeChange, Project: pc.Root, Session: sessID, Target: string(mode), Outcome: "ok", Detail: map[string]any{"from": string(prevMode), "to": string(mode)}})
		}
		modeSet := protocol.ModeSetData{
			SessionID: sessID,
			Mode:      string(mode),
		}
		_ = ipc.SendEvent(protocol.EventModeSet, sessID, env.TurnID, "", modeSet)
		// Every other live peer with this session open must see the change
		// too — not just the connection that sent set_mode. Same reasoning
		// as session_updated/session_deleted below: several peers can have
		// the same session open at once and must sync seamlessly.
		deps.peers.BroadcastEvent(peerID, protocol.EventModeSet, sessID, modeSet)

	case protocol.EventApprovePlan:
		var data protocol.ApprovePlanData
		if err := json.Unmarshal(env.Data, &data); err != nil || !agent.ValidMode(data.Mode) || data.Mode == string(agent.ModePlan) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed approve_plan payload",
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
		// The mode changes first, so what the agent does next runs under it.
		as := func(t protocol.EventType, d any) {
			raw, _ := json.Marshal(d)
			handleEnvelope(ctx, ipc, deps, network, peerID, &protocol.Envelope{Type: t, SessionID: sessID, TurnID: env.TurnID, Data: raw})
		}
		as(protocol.EventSetMode, protocol.SetModeData{SessionID: sessID, Mode: data.Mode, ProjectPath: data.ProjectPath})
		if pc.Registry.Get(ipc, sessID).ResolvePlanApproval(deps.actorName(ipc, peerID)) {
			return // the agent's own turn was waiting on it and carries on
		}
		off := false
		as(protocol.EventUserPrompt, protocol.UserPromptData{
			Prompt: "The plan is approved. Carry it out.", SessionID: sessID, ProjectPath: data.ProjectPath,
			AuthorName: data.AuthorName, AuthorAvatar: data.AuthorAvatar, AutoGenerateTitle: &off,
		})

	case protocol.EventSetWebToolsEnabled:
		var data protocol.SetWebToolsEnabledData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_web_tools_enabled payload",
			})
			return
		}
		pc, err := deps.projects.Resolve(env.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		if sErr := pc.SettingsStore.Save(settings.ProjectSettings{Version: 1, WebToolsEnabled: data.Enabled}); sErr != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("saving settings: %v", sErr),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.SettingsChange, Project: pc.Root, Target: "webToolsEnabled", Outcome: "ok", Detail: map[string]any{"keys": []string{"webToolsEnabled"}, "enabled": data.Enabled}})
		pc.Registry.SetWebToolsEnabled(data.Enabled)
		pc.Terminals.SetAgentNetwork(data.Enabled)
		webToolsSet := protocol.WebToolsEnabledSetData{
			ProjectPath: data.ProjectPath,
			Enabled:     data.Enabled,
		}
		_ = ipc.SendEvent(protocol.EventWebToolsEnabledSet, env.SessionID, env.TurnID, "", webToolsSet)
		// Project-wide, not session-scoped — every peer that might have this
		// project open anywhere needs to know, same as project_list below.
		deps.peers.BroadcastEvent(peerID, protocol.EventWebToolsEnabledSet, "", webToolsSet)

	case protocol.EventAskUserAnswer:
		var data protocol.AskUserAnswerData
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
			pc.Registry.Get(ipc, sessID).HandleAskUserAnswer(env.ToolCallID, data.Answer)
			if strings.HasPrefix(env.ToolCallID, "limit-") { // a soft round/cost limit's Continue / Stop
				outcome := "stop"
				if strings.EqualFold(strings.TrimSpace(data.Answer), "continue") {
					outcome = "continue"
				}
				deps.record(ipc, peerID, audit.Event{Kind: audit.LimitPause, Project: pc.Root, Session: sessID, Outcome: outcome})
			}
		}

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
		deps.record(ipc, peerID, audit.Event{Kind: audit.ProviderAdd, Target: cfg.Name, Outcome: "ok", Detail: map[string]any{
			"httpProvider": cfg.HTTPProvider, "authMethod": cfg.AuthMethod,
		}})
		oauthRegistered := protocol.ProviderRegisteredData{Name: cfg.Name}
		_ = ipc.SendEvent(protocol.EventProviderRegistered, env.SessionID, env.TurnID, "", oauthRegistered)
		deps.peers.BroadcastEvent(peerID, protocol.EventProviderRegistered, "", oauthRegistered)
		reloadRoles(deps.projects.roles, deps.providerStore, deps.settings)

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
		deps.record(ipc, peerID, audit.Event{Kind: audit.ProviderRemove, Target: data.Name, Outcome: "ok"})
		deps.status.ForgetRateLimits(data.Name)
		reloadRoles(deps.projects.roles, deps.providerStore, deps.settings)
		removed := protocol.ProviderRemovedData{Name: data.Name}
		_ = ipc.SendEvent(protocol.EventProviderRemoved, env.SessionID, env.TurnID, "", removed)
		deps.peers.BroadcastEvent(peerID, protocol.EventProviderRemoved, "", removed)

	case protocol.EventListSessions:
		var data protocol.ListSessionsData
		_ = json.Unmarshal(env.Data, &data)

		if data.ProjectPath == protocol.ChatProject {
			_ = ipc.SendEvent(protocol.EventSessionList, env.SessionID, env.TurnID, "", protocol.SessionListData{
				ProjectPath: protocol.ChatProject,
				Sessions:    deps.projects.ListChats(),
				SandboxTier: deps.projects.chatTier,
			})
			return
		}
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
			summaries = append(summaries, sessionSummaryFrom(pc.Root, state))
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
		deps.peers.SetActivity(peerID, data.SessionID, pc.Root)
		state, err := pc.SessStore.Load(data.SessionID)
		var history []agent.Message
		var branches map[string][][]agent.Message
		var mode string
		if err == nil {
			history = state.History
			branches = state.Branches
			mode = state.Mode
			if mode == "" {
				// Empty means the default — say so, or a client keeps its
				// locally seeded mode and shows one the server isn't using.
				mode = string(agent.ModeReadWrite)
			}
		} else if ag := pc.Registry.Lookup(data.SessionID); ag != nil {
			// Not prompted yet, so no file (set_mode doesn't create one) —
			// the live mode is still the truth a client should adopt.
			mode = string(ag.Mode())
		} // a session with no persisted history yet isn't an error — it just has none
		ctxData, spend := pc.Registry.SessionReport(data.SessionID, history)
		_ = ipc.SendEvent(protocol.EventSessionHistory, data.SessionID, env.TurnID, "", protocol.SessionHistoryData{
			SessionID: data.SessionID,
			Messages:  toHistoryMessages(history, branches),
			Mode:      mode,
			Context:   ctxData,
			Spend:     spend,
		})

	case protocol.EventGetContextBreakdown:
		var data protocol.GetContextBreakdownData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed get_context_breakdown payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		var history []agent.Message
		if state, err := pc.SessStore.Load(data.SessionID); err == nil {
			history = state.History
		}
		// A native agent may take a while to count its context, so keep it off the read loop.
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func() {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 19*time.Second)
			defer cancel()
			out := pc.Registry.ContextBreakdown(ctx, data.SessionID, history)
			out.RequestID = data.RequestID
			_ = ipc.SendEvent(protocol.EventContextBreakdown, data.SessionID, env.TurnID, "", out)
		}()

	case protocol.EventCompactContext:
		var data protocol.CompactContextData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed compact_context payload (sessionId is required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, data.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		ag := pc.Registry.Get(ipc, data.SessionID)
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func() {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer recoverTurnPanic(ipc, data.SessionID, env.TurnID)
			if err := ag.Compact(ctx, data.SessionID, env.TurnID, strings.TrimSpace(data.Instructions)); err != nil {
				_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: "compact: " + err.Error()})
			}
		}()

	case protocol.EventEditMessage:
		var data protocol.EditMessageData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" || data.MessageID == "" || data.Content == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed edit_message payload (sessionId, messageId, and content are required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, "", deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		ag := pc.Registry.Get(ipc, data.SessionID)
		author := resolveAuthor(deps, peerID, "", "")
		turnCtx := agent.WithAuthor(ctx, author)
		autoGenerateTitle := deps.settings.GetAutoGenerateTitle()
		if data.AutoGenerateTitle != nil {
			autoGenerateTitle = *data.AutoGenerateTitle
		}
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func(sID, tID, msgID, content string, autoGenerateTitle bool) {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer recoverTurnPanic(ipc, sID, tID)
			if err := ag.EditMessage(turnCtx, sID, tID, msgID, content, autoGenerateTitle); err != nil {
				logError("edit_message turn %s (session %s): %v", tID, sID, err)
				deps.notify.turnFailed(pc, sID, err)
				_ = ipc.SendEvent(protocol.EventError, sID, tID, "", protocol.ErrorData{
					Message: err.Error(),
				})
			}
		}(data.SessionID, env.TurnID, data.MessageID, data.Content, autoGenerateTitle)

	case protocol.EventRetryMessage:
		var data protocol.RetryMessageData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" || data.MessageID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed retry_message payload (sessionId and messageId are required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, "", deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		ag := pc.Registry.Get(ipc, data.SessionID)
		author := resolveAuthor(deps, peerID, "", "")
		turnCtx := agent.WithAuthor(ctx, author)
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func(sID, tID, msgID string) {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer recoverTurnPanic(ipc, sID, tID)
			if err := ag.RetryMessage(turnCtx, sID, tID, msgID); err != nil {
				logError("retry_message turn %s (session %s): %v", tID, sID, err)
				deps.notify.turnFailed(pc, sID, err)
				_ = ipc.SendEvent(protocol.EventError, sID, tID, "", protocol.ErrorData{
					Message: err.Error(),
				})
			}
		}(data.SessionID, env.TurnID, data.MessageID)

	case protocol.EventSwitchBranch:
		var data protocol.SwitchBranchData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" || data.MessageID == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed switch_branch payload (sessionId and messageId are required)",
			})
			return
		}
		pc, err := deps.projects.Resolve(data.SessionID, "", deps.defaultProjectRoot)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		ag := pc.Registry.Get(ipc, data.SessionID)
		if err := ag.SwitchBranch(data.SessionID, data.MessageID); err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{Message: err.Error()})
			return
		}
		state, err := pc.SessStore.Load(data.SessionID)
		var history []agent.Message
		var branches map[string][][]agent.Message
		if err == nil {
			history = state.History
			branches = state.Branches
		}
		ctxData, spend := pc.Registry.SessionReport(data.SessionID, history)
		_ = ipc.SendEvent(protocol.EventSessionHistory, data.SessionID, env.TurnID, "", protocol.SessionHistoryData{
			SessionID: data.SessionID,
			Messages:  toHistoryMessages(history, branches),
			Context:   ctxData,
			Spend:     spend,
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
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(pc.Root, state))
		deps.peers.BroadcastEvent(peerID, protocol.EventSessionUpdated, data.SessionID, sessionSummaryFrom(pc.Root, state))

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
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(pc.Root, state))
		deps.peers.BroadcastEvent(peerID, protocol.EventSessionUpdated, data.SessionID, sessionSummaryFrom(pc.Root, state))

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
		_ = ipc.SendEvent(protocol.EventSessionUpdated, data.SessionID, env.TurnID, "", sessionSummaryFrom(pc.Root, state))
		deps.peers.BroadcastEvent(peerID, protocol.EventSessionUpdated, data.SessionID, sessionSummaryFrom(pc.Root, state))

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
		pc.Terminals.CloseSession(data.SessionID)
		provider.CloseNativeSessions(agent.NativeSessionKey(data.SessionID, pc.Root))
		if err := pc.SessStore.Delete(data.SessionID); err != nil {
			_ = ipc.SendEvent(protocol.EventError, data.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to delete session: %v", err),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.SessionDelete, Project: pc.Root, Session: data.SessionID, Outcome: "ok"})
		deps.projects.Unbind(data.SessionID)
		deps.projects.DropChat(pc.Root)
		_ = ipc.SendEvent(protocol.EventSessionDeleted, data.SessionID, env.TurnID, "", protocol.SessionDeletedData{
			SessionID: data.SessionID,
		})
		deps.peers.BroadcastEvent(peerID, protocol.EventSessionDeleted, data.SessionID, protocol.SessionDeletedData{
			SessionID: data.SessionID,
		})

	case protocol.EventListDirectory:
		var data protocol.ListDirectoryData
		_ = json.Unmarshal(env.Data, &data)
		abs, entries, err := listDirectory(data.Path)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventDirectoryList, env.SessionID, env.TurnID, "", protocol.DirectoryListData{
				Path:    data.Path,
				Entries: []protocol.DirEntry{},
				Error:   err.Error(),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventDirectoryList, env.SessionID, env.TurnID, "", protocol.DirectoryListData{
			Path:    abs,
			Entries: entries,
		})

	case protocol.EventListPermissionRules, protocol.EventAddPermissionRule, protocol.EventDeletePermissionRule:
		handlePermissionRuleRequest(ipc, deps, peerID, env)

	case protocol.EventListMCPServers, protocol.EventSaveMCPServer, protocol.EventRemoveMCPServer, protocol.EventSetMCPServerEnabled,
		protocol.EventSetMCPToolEnabled, protocol.EventReconnectMCPServer, protocol.EventMCPOAuthStart, protocol.EventMCPOAuthComplete, protocol.EventMCPSignOut,
		protocol.EventImportMCPServers:
		handleMCPRequest(ctx, ipc, deps, peerID, env)

	case protocol.EventScanMCPImports:
		handleMCPScan(ipc, deps, env)

	case protocol.EventListSkills, protocol.EventSetSkillEnabled, protocol.EventGetSkill:
		handleSkillsRequest(ipc, deps, peerID, env)

	case protocol.EventListAudit:
		handleListAudit(ipc, deps, env)

	case protocol.EventListNotifications:
		_ = ipc.SendEvent(protocol.EventNotificationList, "", "", "", protocol.NotificationListData{Notifications: deps.notify.List()})

	case protocol.EventGetUsage:
		var data protocol.GetUsageData
		_ = json.Unmarshal(env.Data, &data)
		var since time.Time
		if data.Since != "" {
			since, _ = time.Parse(time.RFC3339, data.Since)
		}
		kinds := map[string]string{}
		if deps.providerStore != nil {
			for _, p := range deps.providerStore.List() {
				kinds[p.Name] = string(p.Kind)
			}
		}
		// A cold scan reads every session file, so keep it off the read loop.
		go func() {
			var price usagePricer
			if deps.pricing != nil {
				price = func(model string, in, cr, cw, o int) float64 {
					usd, _ := deps.pricing.CostUSD(model, in, cr, cw, o)
					return usd
				}
			}
			out := buildUsageReport(deps.projects.stateDir, kinds, since, data.External, price)
			out.Accounts = usageAccounts(deps.providerStore)
			out.RequestID = data.RequestID
			_ = ipc.SendEvent(protocol.EventUsageReport, "", "", "", out)
		}()

	case protocol.EventDismissNotification:
		var data protocol.DismissNotificationData
		_ = json.Unmarshal(env.Data, &data)
		deps.notify.Dismiss(data.ID)

	case protocol.EventMarkNotificationsRead:
		var data protocol.MarkNotificationsReadData
		_ = json.Unmarshal(env.Data, &data)
		deps.notify.MarkRead(data.IDs...)

	case protocol.EventSetPresence:
		var data protocol.SetPresenceData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return
		}
		if deps.peers.SetPresence(peerID, data.SessionIDs, data.Focused, data.Visible) && data.Focused && data.Visible {
			for _, id := range data.SessionIDs {
				deps.notify.MarkSessionRead(id) // looking at a chat is reading it
			}
		}

	case protocol.EventSetNotifyPrefs:
		var data protocol.NotifyPrefsData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed set_notify_prefs payload",
			})
			return
		}
		stored, err := deps.settings.SetNotifyPrefs(data)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to save notification settings: %v", err),
			})
			return
		}
		deps.peers.BroadcastEvent("", protocol.EventNotifyPrefs, "", stored)

	case protocol.EventUploadBegin:
		var data protocol.UploadBeginData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{Error: "malformed upload_begin payload"})
			return
		}
		path, done, err := deps.uploads.Begin(peerID, data)
		switch {
		case err != nil:
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{UploadID: data.UploadID, Error: err.Error()})
		case done:
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{UploadID: data.UploadID, Path: path})
		default:
			_ = ipc.SendEvent(protocol.EventUploadProgress, "", "", "", protocol.UploadProgressData{UploadID: data.UploadID})
		}

	case protocol.EventUploadChunk:
		var data protocol.UploadChunkData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{Error: "malformed upload_chunk payload"})
			return
		}
		path, received, err := deps.uploads.Chunk(peerID, data)
		switch {
		case err != nil:
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{UploadID: data.UploadID, Error: err.Error()})
		case path != "":
			_ = ipc.SendEvent(protocol.EventUploadDone, "", "", "", protocol.UploadDoneData{UploadID: data.UploadID, Path: path})
		default:
			// The ack doubles as flow control: a client sends its next chunk
			// once this arrives, so a big upload never queues ahead of chat.
			_ = ipc.SendEvent(protocol.EventUploadProgress, "", "", "", protocol.UploadProgressData{UploadID: data.UploadID, Received: received})
		}

	case protocol.EventImportChatFiles:
		var data protocol.ImportChatFilesData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.SessionID == "" || data.Target == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed import_chat_files payload (sessionId and target are required)",
			})
			return
		}
		reply := func(out protocol.ChatFilesImportedData) {
			out.RequestID, out.SessionID = data.RequestID, data.SessionID
			_ = ipc.SendEvent(protocol.EventChatFilesImported, data.SessionID, env.TurnID, "", out)
		}
		chat, err := deps.projects.Resolve(data.SessionID, protocol.ChatProject, "")
		if err == nil {
			_, err = chat.SessStore.Load(data.SessionID)
		}
		if err != nil {
			reply(protocol.ChatFilesImportedData{Error: "no such chat"})
			return
		}
		target, err := projectKey(data.Target)
		if err != nil || deps.projects.isChat(target) || !deps.projects.Known(target) {
			reply(protocol.ChatFilesImportedData{Error: "target must be a project this server knows"})
			return
		}
		go func() {
			dir, files, size, err := importChatFiles(chat.Root, target, data.SessionID)
			out := protocol.ChatFilesImportedData{Target: target, Dir: dir, Files: files, Bytes: size}
			outcome := "ok"
			if err != nil {
				out, outcome = protocol.ChatFilesImportedData{Target: target, Error: err.Error()}, "error"
			}
			deps.record(ipc, peerID, audit.Event{Kind: audit.FsOp, Project: target, Session: data.SessionID, Tool: "import_chat_files", Target: out.Dir, Outcome: outcome})
			reply(out)
		}()

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
			files := make([]string, 0, len(snap.Files))
			for rel := range snap.Files {
				files = append(files, filepath.ToSlash(rel))
			}
			sort.Strings(files)
			summaries = append(summaries, protocol.SnapshotSummary{
				SnapshotID:  snap.ID,
				Description: snap.Description,
				CreatedAt:   snap.CreatedAt,
				Files:       files,
			})
		}
		_ = ipc.SendEvent(protocol.EventSnapshotList, env.SessionID, env.TurnID, "", protocol.SnapshotListData{
			ProjectPath: pc.Root,
			Snapshots:   summaries,
		})

	case protocol.EventTerminalOpen:
		var data protocol.TerminalOpenData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed terminal_open payload",
			})
			return
		}
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		snap, err := pc.Terminals.Open(ctx, ipc, sessID, data.TerminalID, data.Cwd, data.Cols, data.Rows, data.Env)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventTerminalError, sessID, env.TurnID, "", protocol.TerminalErrorData{
				TerminalID: data.TerminalID,
				Message:    err.Error(),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.TerminalOpen, Project: pc.Root, Session: sessID, Target: data.Cwd, Outcome: "ok", Detail: map[string]any{"terminalId": data.TerminalID}})
		_ = ipc.SendEvent(protocol.EventTerminalSnapshot, sessID, env.TurnID, "", snap)

	case protocol.EventTerminalAttach:
		var data protocol.TerminalAttachData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed terminal_attach payload",
			})
			return
		}
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		snap, err := pc.Terminals.Attach(ctx, ipc, sessID, data.TerminalID, data.Cols, data.Rows, data.RestartIfExited)
		if err != nil {
			_ = ipc.SendEvent(protocol.EventTerminalError, sessID, env.TurnID, "", protocol.TerminalErrorData{
				TerminalID: data.TerminalID,
				Message:    err.Error(),
			})
			return
		}
		_ = ipc.SendEvent(protocol.EventTerminalSnapshot, sessID, env.TurnID, "", snap)

	case protocol.EventTerminalInput:
		var data protocol.TerminalInputData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return
		}
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		if err := pc.Terminals.Input(sessID, data.TerminalID, data.Data); err != nil {
			_ = ipc.SendEvent(protocol.EventTerminalError, sessID, env.TurnID, "", protocol.TerminalErrorData{
				TerminalID: data.TerminalID,
				Message:    err.Error(),
			})
		}

	case protocol.EventTerminalResize:
		var data protocol.TerminalResizeData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return
		}
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		if err := pc.Terminals.Resize(sessID, data.TerminalID, data.Cols, data.Rows); err != nil {
			_ = ipc.SendEvent(protocol.EventTerminalError, sessID, env.TurnID, "", protocol.TerminalErrorData{
				TerminalID: data.TerminalID,
				Message:    err.Error(),
			})
		}

	case protocol.EventTerminalClose:
		var data protocol.TerminalCloseData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return
		}
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		if err := pc.Terminals.Close(sessID, data.TerminalID); err != nil {
			_ = ipc.SendEvent(protocol.EventTerminalError, sessID, env.TurnID, "", protocol.TerminalErrorData{
				TerminalID: data.TerminalID,
				Message:    err.Error(),
			})
		}

	case protocol.EventBrowserHost:
		var data protocol.BrowserHostData
		if json.Unmarshal(env.Data, &data) == nil && deps.peers != nil {
			deps.peers.SetBrowserHost(peerID, data.Enabled)
		}

	case protocol.EventBrowserResponse:
		var data protocol.BrowserResponseData
		if json.Unmarshal(env.Data, &data) == nil {
			deps.browser.Respond(peerID, data)
		}

	case protocol.EventListTerminals:
		var data protocol.ListTerminalsData
		_ = json.Unmarshal(env.Data, &data)
		pc, sessID, err := resolveTerminalSession(ipc, deps, env, data.SessionID, data.ProjectPath)
		if err != nil {
			return
		}
		_ = ipc.SendEvent(protocol.EventTerminalList, sessID, env.TurnID, "", protocol.TerminalListData{
			SessionID: sessID,
			Terminals: pc.Terminals.List(sessID),
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
		if st := deps.projects.GitStatus(pc.Root); st != nil {
			_ = ipc.SendEvent(protocol.EventGitStatus, "", "", "", *st)
		}
		deps.peers.BroadcastEvent(peerID, protocol.EventProjectList, "", protocol.ProjectListData{
			Paths: deps.projects.List(),
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
		deps.record(ipc, peerID, audit.Event{Kind: audit.ProjectRemove, Project: key, Outcome: "ok"})
		_ = ipc.SendEvent(protocol.EventProjectRemoved, env.SessionID, env.TurnID, "", protocol.ProjectRemovedData{
			Path: key,
		})
		deps.peers.BroadcastEvent(peerID, protocol.EventProjectRemoved, "", protocol.ProjectRemovedData{
			Path: key,
		})

	case protocol.EventDetectCLI:
		agents := map[string]bool{}
		for _, k := range provider.CLIKinds() {
			agents["cli_"+string(k)] = provider.DetectCLI(k)
		}
		_ = ipc.SendEvent(protocol.EventCLIAvailability, env.SessionID, env.TurnID, "", protocol.CLIAvailabilityData{
			Claude: agents["cli_claude"],
			Codex:  agents["cli_codex"],
			Agents: agents,
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
		targetName, targetIP, _, _ := deps.peers.Identify(data.PeerID)
		if !deps.peers.Disconnect(data.PeerID) {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("unknown peer %q", data.PeerID),
			})
		} else {
			deps.record(ipc, peerID, audit.Event{Kind: audit.PeerKick, Target: targetName, Outcome: "ok", Detail: map[string]any{"targetIp": targetIP}})
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
			_ = p.ipc.SendEvent(protocol.EventServerIdentity, "", "", "", protocol.ServerIdentityData{Identity: data.Name, ServerID: deps.settings.GetServerID(), Version: version})
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
		cfg, rErr := applyRemoteAccess(ctx, deps, data)
		if rErr != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to enable remote access: %v", rErr),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.RemoteAccess, Outcome: "ok", Detail: map[string]any{"enabled": cfg.Enabled, "addr": cfg.Addr}})
		_ = ipc.SendEvent(protocol.EventRemoteAccessSet, env.SessionID, env.TurnID, "", remoteAccessData(cfg, true))
		broadcastRemoteAccess(deps, peerID)

	case protocol.EventSetRuntimeSettings:
		handleSetRuntimeSettings(ipc, env, peerID, deps)

	case protocol.EventSetDefaultProvider:
		handleSetDefaultProvider(ipc, env, peerID, deps)

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
		// It's the TCP token, and failed logins aren't rate-limited.
		if len(data.Password) < minRemotePasswordLen {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("remote password must be at least %d characters", minRemotePasswordLen),
			})
			return
		}
		if err := deps.settings.SetToken(data.Password); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to save remote password: %v", err),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.RemotePassword, Outcome: "ok"}) // the fact, never the password
		_ = ipc.SendEvent(protocol.EventRemotePasswordSet, env.SessionID, env.TurnID, "", protocol.RemotePasswordSetData{})

	case protocol.EventApprovePairing:
		if network != "unix" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "approve_pairing is local-socket-only",
			})
			return
		}
		var data protocol.ApprovePairingData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Credential == "" {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: "malformed approve_pairing payload (credential is required)",
			})
			return
		}
		deps.pairings.answer(data.Credential, data.Allow)

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
		add := deps.settings.AddToken
		if data.Pairing {
			add = deps.settings.AddPairingToken
		}
		if err := add(data.Name, token, data.Email); err != nil {
			_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
				Message: err.Error(),
			})
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.TokenAdd, Target: data.Name, Outcome: "ok"}) // the name, never the token
		// Best-effort: a client can still pair by pasting the CA separately
		// (or doesn't need one at all, on an operator-supplied cert) if this
		// fails, so a CA read error shouldn't fail token registration itself.
		caPem, _ := deps.remote.CAPEM()
		_ = ipc.SendEvent(protocol.EventTokenRegistered, env.SessionID, env.TurnID, "", protocol.TokenRegisteredData{
			Name:  data.Name,
			Token: token,
			Email: data.Email,
			CaPem: caPem,

			RTCFingerprint: deps.remote.RTCFingerprint(),
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
		// Revocation should actually end the session, not just block the next
		// handshake — otherwise someone already connected on this credential
		// stays connected until they happen to disconnect on their own.
		dropped := deps.peers.DisconnectByToken(data.Name)
		deps.record(ipc, peerID, audit.Event{Kind: audit.TokenRemove, Target: data.Name, Outcome: "ok", Detail: map[string]any{"disconnected": dropped}})
		_ = ipc.SendEvent(protocol.EventTokenRemoved, env.SessionID, env.TurnID, "", protocol.TokenRemovedData{
			Name: data.Name,
		})
	}
}
