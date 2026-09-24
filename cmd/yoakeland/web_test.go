package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yoakeland/internal/protocol"
)

// startWebTestServer runs acceptLoop over a real TLS listener, with a
// throwaway web root, the way remoteControl.Start does.
func startWebTestServer(t *testing.T) (addr string, token string) {
	t.Helper()
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html>index"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log(1)"), 0o644)

	deps := newTestDeps(t)
	deps.webRoot = root
	token = "web-token"
	lookup := func(tok string) (string, TokenRecord, bool) {
		if tok == token {
			return "default", TokenRecord{Token: token}, true
		}
		return "", TokenRecord{}, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go acceptLoop(ctx, wrapTLS(plainLn, cert), "tcp", deps, lookup, newConnTracker())
	return plainLn.Addr().String(), token
}

func insecureClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func mustEnvelope(t *testing.T, typ protocol.EventType, data any) []byte {
	t.Helper()
	raw, _ := json.Marshal(data)
	env, _ := json.Marshal(protocol.Envelope{Type: typ, Data: raw})
	return append(env, '\n')
}

func TestWeb_ServesUIWithSPAFallback(t *testing.T) {
	addr, _ := startWebTestServer(t)
	for path, want := range map[string]string{"/app.js": "console.log(1)", "/": "index", "/some/route": "index"} {
		resp, err := insecureClient().Get("https://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s = %q, want it to contain %q", path, body, want)
		}
	}
}

func TestWeb_WebSocketSpeaksTheProtocol(t *testing.T) {
	addr, token := startWebTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "wss://"+addr+"/ws", &websocket.DialOptions{
		HTTPClient: insecureClient(),
		HTTPHeader: http.Header{"Origin": {"https://" + addr}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	_ = ws.Write(ctx, websocket.MessageText, mustEnvelope(t, protocol.EventAuth, protocol.AuthData{Token: token}))
	_ = ws.Write(ctx, websocket.MessageText, mustEnvelope(t, protocol.EventPing, protocol.PingData{Nonce: "over-ws"}))
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			t.Fatalf("message isn't one JSON frame: %q", msg)
		}
		if env.Type == protocol.EventPong {
			var pong protocol.PongData
			_ = json.Unmarshal(env.Data, &pong)
			if pong.Nonce != "over-ws" {
				t.Fatalf("pong nonce = %q", pong.Nonce)
			}
			return
		}
	}
}

func TestWeb_WebSocketRejectsOtherOrigins(t *testing.T) {
	addr, _ := startWebTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "wss://"+addr+"/ws", &websocket.DialOptions{
		HTTPClient: insecureClient(),
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("a cross-origin WebSocket was accepted")
	}
}

// The peek that routes browsers to HTTP must not disturb ordinary clients.
func TestWeb_PlainTLSClientsStillWork(t *testing.T) {
	addr, token := startWebTestServer(t)
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write(mustEnvelope(t, protocol.EventAuth, protocol.AuthData{Token: token}))
	_, _ = conn.Write(mustEnvelope(t, protocol.EventPing, protocol.PingData{Nonce: "plain"}))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env protocol.Envelope
		_ = json.Unmarshal(line, &env)
		if env.Type == protocol.EventPong {
			return
		}
	}
}
