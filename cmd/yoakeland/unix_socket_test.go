//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/protocol"
)

// runtimeDirEnvVar returns the OS-specific env var sockpath.DefaultRuntimeDir
// checks first (XDG_RUNTIME_DIR everywhere but macOS, TMPDIR there).
func runtimeDirEnvVar() string {
	if runtime.GOOS == "darwin" {
		return "TMPDIR"
	}
	return "XDG_RUNTIME_DIR"
}

// TestParseListenAddr guards the switch between TCP (token-authenticated)
// and unix socket (permission-authenticated) server modes, including the
// bare "unix"/"unix:" shorthand for the default socket path — the point
// being that a local caller shouldn't have to spell out a path at all. With
// no OS runtime dir set, that default falls back to stateDir.
func TestParseListenAddr(t *testing.T) {
	const stateDir = "/home/user/.yoake"
	t.Setenv(runtimeDirEnvVar(), "")
	cases := []struct {
		in, wantNetwork, wantAddress string
	}{
		{":7890", "tcp", ":7890"},
		{"127.0.0.1:7890", "tcp", "127.0.0.1:7890"},
		{"unix:/tmp/yoakeland.sock", "unix", "/tmp/yoakeland.sock"},
		{"unix:relative/path.sock", "unix", "relative/path.sock"},
		{"unix", "unix", "/home/user/.yoake/yoakeland.sock"},
		{"unix:", "unix", "/home/user/.yoake/yoakeland.sock"},
	}
	for _, tc := range cases {
		network, address := parseListenAddr(tc.in, stateDir)
		if network != tc.wantNetwork || address != tc.wantAddress {
			t.Errorf("parseListenAddr(%q, %q) = (%q, %q), want (%q, %q)", tc.in, stateDir, network, address, tc.wantNetwork, tc.wantAddress)
		}
	}
}

// TestParseListenAddr_PrefersRuntimeDir guards that the bare "unix" default
// prefers the OS's own per-session runtime directory over stateDir when one
// is set (see sockpath.DefaultRuntimeDir) — a socket is ephemeral runtime
// state, not persistent data, and shouldn't default to living next to
// providers.json when a better-suited directory is available.
func TestParseListenAddr_PrefersRuntimeDir(t *testing.T) {
	t.Setenv(runtimeDirEnvVar(), "/run/user/1000")

	network, address := parseListenAddr("unix", "/home/user/.yoake")
	if network != "unix" {
		t.Fatalf("expected unix network, got %q", network)
	}
	want := filepath.Join("/run/user/1000", "yoake", "yoakeland.sock")
	if address != want {
		t.Fatalf("parseListenAddr(\"unix\", ...) = %q, want %q (should prefer the OS runtime dir over stateDir)", address, want)
	}
}

// TestServerMode_UnixSocketNeedsNoToken guards the actual point of the unix
// socket listener: a client can dial it and get a dispatch round-trip with
// no `auth` frame at all, and the socket file itself is created owner-only
// (0600) so the OS is the thing enforcing who may connect.
func TestServerMode_UnixSocketNeedsNoToken(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "yoakeland.sock")

	network, address := parseListenAddr("unix:"+sockPath, "")
	if network != "unix" {
		t.Fatalf("expected unix network, got %q", network)
	}

	ln, err := net.Listen(network, address)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	if err := os.Chmod(address, 0600); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}

	info, err := os.Stat(address)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("socket file permissions = %o, want 0600", perm)
	}

	deps := newTestDeps(t)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		ipc := protocol.NewIPCStream(conn, conn)
		// No auth frame sent by the client below, deliberately — this is the
		// behavior under test: a unix-socket connection needs no handshake
		// at all, so its first envelope is handled directly.
		serveConnection(t.Context(), ipc, deps, "unix", "", nil)
	}()

	clientConn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer clientConn.Close()

	pingData, _ := json.Marshal(protocol.PingData{Nonce: "no-token-needed"})
	raw, _ := json.Marshal(protocol.Envelope{Type: protocol.EventPing, Data: pingData})
	if _, err := clientConn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(clientConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading pong over unix socket: %v", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}
	if env.Type != protocol.EventPong {
		t.Fatalf("expected pong with no auth frame sent, got %s (line: %s)", env.Type, line)
	}
	var pong protocol.PongData
	_ = json.Unmarshal(env.Data, &pong)
	if pong.Nonce != "no-token-needed" {
		t.Fatalf("expected pong to echo the nonce, got %+v", pong)
	}
}

// TestSetupUnixListener_CreatesOwnerOnlySocket guards the daemon-mode setup
// path itself (as opposed to TestServerMode_UnixSocketNeedsNoToken, which
// builds the listener by hand): the parent directory is created, the socket
// file ends up 0600, and a client can dial it end to end via acceptLoop with
// no auth frame.
func TestSetupUnixListener_CreatesOwnerOnlySocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nested", "yoakeland.sock")

	ln, err := setupUnixListener(sockPath, true)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("socket file permissions = %o, want 0600", perm)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go acceptLoop(ctx, ln, "unix", newTestDeps(t), nil, nil)

	clientConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer clientConn.Close()

	pingData, _ := json.Marshal(protocol.PingData{Nonce: "daemon-mode"})
	raw, _ := json.Marshal(protocol.Envelope{Type: protocol.EventPing, Data: pingData})
	if _, err := clientConn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(clientConn)

	// A freshly accepted connection also gets an unsolicited peer_list and
	// server_identity push (peer.go/settings.go) ahead of any response to
	// what the client itself sent — skip past those to find the pong.
	for i := 0; i < 20; i++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reading over unix socket: %v", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("unmarshaling response: %v (line: %s)", err, line)
		}
		if env.Type == protocol.EventPong {
			return
		}
	}
	t.Fatal("expected a pong with no auth frame sent, never saw one")
}

// TestSetupTCPListener_GeneratesTokenWhenEmpty guards that leaving -token
// unset still ends up with a real token gating the listener, rather than an
// empty string that would make handshake() trivially satisfiable.
func TestSetupTCPListener_GeneratesTokenWhenEmpty(t *testing.T) {
	ln, token, err := setupTCPListener("127.0.0.1:0", t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("setupTCPListener: %v", err)
	}
	defer ln.Close()

	if token == "" {
		t.Fatal("expected a non-empty auto-generated token")
	}
}

// The socket's directory is tightened to owner-only, a live server's socket
// is never taken over, and a non-socket file at the path is never deleted.
func TestSetupUnixListener_Hardening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only")
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "yoakeland.sock")
	ln, err := setupUnixListener(sock, true)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	defer ln.Close()
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", fi.Mode().Perm())
	}

	if _, err := setupUnixListener(sock, true); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second listener on a live socket: err = %v", err)
	}

	// A crashed run's socket (nothing answering) is replaced.
	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	sl.Close()
	ln2, err := setupUnixListener(stale, true)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()

	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setupUnixListener(file, true); err == nil {
		t.Fatal("expected refusal to replace a regular file")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep me" {
		t.Fatal("regular file was touched")
	}

	// An override's parent (not ours to manage) keeps its mode.
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	ln3, err := setupUnixListener(filepath.Join(shared, "y.sock"), false)
	if err != nil {
		t.Fatal(err)
	}
	ln3.Close()
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Fatalf("override dir mode changed to %o", fi.Mode().Perm())
	}
}
