package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tlsca"
)

// writeSelfSignedCert generates a throwaway self-signed cert/key pair valid
// for "127.0.0.1" and writes them as PEM files, returning their paths.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yoakeland-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("creating cert file: %v", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encoding cert: %v", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("creating key file: %v", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encoding key: %v", err)
	}

	return certPath, keyPath
}

// TestServerMode_TLSEndToEnd guards the whole TLS path for real: a plain
// listener wrapped by wrapTLS, a client dialing with tls.Dial, the auth
// handshake, and a dispatch round-trip, all over the encrypted connection.
func TestServerMode_TLSEndToEnd(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())

	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	tlsLn := wrapTLS(plainLn, cert)
	defer tlsLn.Close()

	deps := newTestDeps(t)
	const token = "test-token"

	go func() {
		conn, err := tlsLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		ipc := protocol.NewIPCStream(conn, conn)
		lookup := func(tok string) (string, TokenRecord, bool) {
			if tok == token {
				return "default", TokenRecord{Token: token}, true
			}
			return "", TokenRecord{}, false
		}
		_, _, ok := handshake(ipc, "tcp", lookup)
		if !ok {
			return
		}
		serveConnection(t.Context(), ipc, deps, "tcp", "", nil)
	}()

	clientConn, err := tls.Dial("tcp", tlsLn.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer clientConn.Close()

	send := func(env protocol.Envelope) {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := clientConn.Write(append(raw, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	authData, _ := json.Marshal(protocol.AuthData{Token: token})
	send(protocol.Envelope{Type: protocol.EventAuth, Data: authData})

	pingData, _ := json.Marshal(protocol.PingData{Nonce: "over-tls"})
	send(protocol.Envelope{Type: protocol.EventPing, Data: pingData})

	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(clientConn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading pong over TLS: %v", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}
	if env.Type != protocol.EventPong {
		t.Fatalf("expected pong, got %s (line: %s)", env.Type, line)
	}
	var pong protocol.PongData
	_ = json.Unmarshal(env.Data, &pong)
	if pong.Nonce != "over-tls" {
		t.Fatalf("expected pong to echo the nonce, got %+v", pong)
	}
}

// TestResolveServerCert_BadCertPath guards the error path: an explicit
// -tls-cert/-tls-key pointing at a missing file must fail loudly instead of
// silently falling back to the auto-managed CA (or, worse, plaintext).
func TestResolveServerCert_BadCertPath(t *testing.T) {
	_, _, _, _, err := resolveServerCert(t.TempDir(), "/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected an error resolving a nonexistent cert/key pair")
	}
}

// TestResolveServerCert_DefaultsToAutoCA guards the new default behavior:
// with no -tls-cert/-tls-key, resolveServerCert must succeed using
// yoakeland's own self-managed CA rather than requiring an operator-supplied
// certificate (the old plaintext-fallback behavior this replaces).
func TestResolveServerCert_DefaultsToAutoCA(t *testing.T) {
	cert, usingAutoCA, caPEMPath, fingerprint, err := resolveServerCert(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("resolveServerCert with no explicit cert: %v", err)
	}
	if !usingAutoCA {
		t.Error("expected usingAutoCA to be true when no -tls-cert/-tls-key is given")
	}
	if len(cert.Certificate) == 0 {
		t.Error("expected a non-empty certificate chain")
	}
	if caPEMPath == "" || fingerprint == "" {
		t.Errorf("expected a CA PEM path and fingerprint, got path=%q fingerprint=%q", caPEMPath, fingerprint)
	}
	if _, err := os.Stat(caPEMPath); err != nil {
		t.Errorf("expected the CA PEM file to exist at %s: %v", caPEMPath, err)
	}
}

// acceptFailer fails every Accept, the way a process out of file descriptors does.
type acceptFailer struct{ calls atomic.Int64 }

func (a *acceptFailer) Accept() (net.Conn, error) {
	a.calls.Add(1)
	return nil, errors.New("accept tcp: too many open files")
}
func (a *acceptFailer) Close() error   { return nil }
func (a *acceptFailer) Addr() net.Addr { return &net.TCPAddr{} }

// A listener whose Accept keeps failing must be retried with backoff, not in a
// busy loop that logs on every attempt (NET-8).
func TestAcceptLoop_BacksOffOnAcceptErrors(t *testing.T) {
	ln := &acceptFailer{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	acceptLoop(ctx, ln, "unix", connDeps{}, nil, nil) // returns once ctx expires
	if n := ln.calls.Load(); n > 20 {
		t.Fatalf("%d Accept calls in 200ms: a busy retry loop, not backoff", n)
	}
}

// With every TLS handshake slot taken (maxHandshakes connections already mid-handshake),
// a new connection is dropped before it costs a handshake (NET-8).
func TestAcceptLoop_DropsTLSConnsOverHandshakeCap(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go acceptLoop(ctx, wrapTLS(plainLn, cert), "tcp", newTestDeps(t), nil, newConnTracker())

	for i := 0; i < maxHandshakes; i++ {
		handshakeSlots <- struct{}{}
	}
	defer func() {
		for len(handshakeSlots) > 0 {
			<-handshakeSlots
		}
	}()

	raw, err := net.DialTimeout("tcp", plainLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	client := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := client.Handshake(); err == nil {
		t.Fatal("handshake succeeded with every handshake slot taken")
	}
}

// A leaf close to expiry is regenerated on the next handshake, so a daemon that has been up
// for a year doesn't keep serving an expired certificate (NET-18).
func TestRenewingCert_RegeneratesNearExpiryLeaf(t *testing.T) {
	stateDir := t.TempDir()
	if _, _, _, err := tlsca.EnsureServerCert(stateDir, tlsca.DefaultSANs()); err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}

	// Re-sign the leaf with the same CA so it expires within the hour.
	dir := filepath.Join(stateDir, "tls")
	caBlock, _ := pem.Decode(mustRead(t, filepath.Join(dir, "ca.pem")))
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyBlock, _ := pem.Decode(mustRead(t, filepath.Join(dir, "ca.key")))
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "yoakeland"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "leaf.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "leaf.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}), 0o600)
	expiring, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}))
	if err != nil {
		t.Fatal(err)
	}

	got, err := renewingCert(stateDir, expiring)(nil)
	if err != nil {
		t.Fatalf("renewingCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(leaf.NotAfter) < 300*24*time.Hour {
		t.Fatalf("served a leaf expiring %v from now, want a renewed one", time.Until(leaf.NotAfter))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
