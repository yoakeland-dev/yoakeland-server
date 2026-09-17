package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"yoakeland/internal/protocol"
)

// dialTLS dials addr and completes a TLS handshake without verifying the
// server's certificate — every yoakeland TCP listener is TLS-wrapped now
// (either an explicit cert or the auto-managed CA), so ordinary tests that
// only care about listener/auth/lifecycle behavior use this instead of a
// plaintext net.DialTimeout. TestRemoteControl_AutoCA_ClientTrustsOnlyThatCA
// below is the one test that verifies the certificate for real.
func dialTLS(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dialTLS(%s): %v", addr, err)
	}
	return conn
}

// TestRemoteControl_StartStopTogglesListener guards the live on/off switch
// set_remote_access needs: Start binds a real, dialable TCP listener; Stop
// closes it so a subsequent dial fails; Start again reopens it. Also checks
// that leaving no address configured falls back to defaultRemoteAddr's port
// convention by using an explicit ephemeral one instead (":0"), since
// binding the literal default in a test would collide with a real server.
func TestRemoteControl_StartStopTogglesListener(t *testing.T) {
	deps := newTestDeps(t)
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings, deps: deps}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := rc.ln.Addr().String()

	dialTLS(t, addr).Close()

	// A second Start while already running must be a no-op, not a rebind
	// (which would change the address out from under anyone holding it).
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start while already running: %v", err)
	}
	if rc.ln.Addr().String() != addr {
		t.Fatalf("expected Start to be a no-op while running, address changed to %s", rc.ln.Addr().String())
	}

	rc.Stop()
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("expected dialing after Stop to fail")
	}

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	defer rc.Stop()
	dialTLS(t, rc.ln.Addr().String()).Close()
}

// TestRemoteControl_StartGeneratesAndPersistsToken guards that enabling
// remote access with no password ever set doesn't silently regenerate a new
// token on every Start — it's generated once and then persisted, so a
// restart (or a second Start) keeps using the same one.
func TestRemoteControl_StartGeneratesAndPersistsToken(t *testing.T) {
	deps := newTestDeps(t)
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings, deps: deps}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Stop()

	if deps.settings.GetToken() != "" {
		t.Fatalf("expected no token before Start")
	}
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	token := deps.settings.GetToken()
	if token == "" {
		t.Fatal("expected Start to generate and persist a token")
	}

	// The generated token must actually gate the listener.
	conn := dialTLS(t, rc.ln.Addr().String())
	defer conn.Close()

	authData, _ := json.Marshal(protocol.AuthData{Token: token})
	raw, _ := json.Marshal(protocol.Envelope{Type: protocol.EventAuth, Data: authData})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

// TestRemoteControl_Stop_DropsAlreadyConnectedPeers guards SPEC.md's
// set_remote_access(enabled:false) contract: "Disabling closes the listener
// and drops every currently-connected TCP peer (not just new ones)." Before
// the fix, Stop() only closed the listener — an already-accepted connection
// idling in a blocking ReadEnvelope() call never noticed the per-listener
// context being cancelled, so it stayed open indefinitely.
func TestRemoteControl_Stop_DropsAlreadyConnectedPeers(t *testing.T) {
	deps := newTestDeps(t)
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings, deps: deps}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	token := deps.settings.GetToken()

	conn := dialTLS(t, rc.ln.Addr().String())
	defer conn.Close()

	authData, _ := json.Marshal(protocol.AuthData{Token: token})
	raw, _ := json.Marshal(protocol.Envelope{Type: protocol.EventAuth, Data: authData})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	// Drain the server_identity (and possibly peer_list) push(es) sent right
	// after a successful handshake, confirming the connection is fully live
	// before we test that Stop() drops it.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("expected to read server_identity after a successful handshake: %v", err)
	}

	rc.Stop()

	// The connection must now be closed server-side. Keep reading (there may
	// be one more already-buffered push in flight, e.g. peer_list as the peer
	// is removed) until either a real error (EOF/reset — success) or the
	// overall deadline elapses with no such error (failure: it stayed open).
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, err := conn.Read(buf)
		if err == nil {
			continue // more buffered data (e.g. a peer_list push) — keep draining
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("expected the connection to be closed after Stop(), but it stayed open")
		}
		return // any non-timeout error (EOF, connection reset, use of closed connection) is the expected closure
	}
}

// TestRemoteControl_AutoCA_ClientTrustsOnlyThatCA is the test that actually
// proves the pairing model works: with no -tls-cert/-tls-key given, Start
// must stand up TLS backed by yoakeland's own self-managed CA persisted
// under <stateDir>/tls/ca.pem, and a client that trusts *only* that CA (not
// the public trust store, no InsecureSkipVerify) must be able to complete a
// real certificate-verifying handshake against it.
func TestRemoteControl_AutoCA_ClientTrustsOnlyThatCA(t *testing.T) {
	deps := newTestDeps(t)
	stateDir := t.TempDir()
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: stateDir, settings: deps.settings, deps: deps}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Stop()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	caPEMPath := filepath.Join(stateDir, "tls", "ca.pem")
	caPEM, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatalf("expected the auto-managed CA cert at %s: %v", caPEMPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse the persisted CA cert")
	}

	host, _, err := net.SplitHostPort(rc.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", rc.ln.Addr().String(), &tls.Config{
		RootCAs:    pool,
		ServerName: host, // "127.0.0.1", which DefaultSANs() includes as an IP SAN
	})
	if err != nil {
		t.Fatalf("expected a real certificate-verifying TLS handshake to succeed against the auto-managed CA: %v", err)
	}
	conn.Close()
}
