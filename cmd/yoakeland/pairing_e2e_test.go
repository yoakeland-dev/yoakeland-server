package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// signIn sends an auth frame and returns this connection's own row from the first peer_list,
// or the error message if the server refused it.
func signIn(t *testing.T, c net.Conn, auth protocol.AuthData) (me protocol.PeerInfo, refused string) {
	t.Helper()
	b, _ := json.Marshal(protocol.Envelope{Type: protocol.EventAuth, Data: mustJSON(t, auth)})
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(c, 1<<20)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no peer_list: %v", err)
		}
		var env protocol.Envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.EventError:
			var e protocol.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			return me, e.Message
		case protocol.EventPeerList:
			var pl protocol.PeerListData
			_ = json.Unmarshal(env.Data, &pl)
			for _, p := range pl.Peers {
				if p.PeerID == pl.You {
					return p, ""
				}
			}
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The whole pairing path a phone takes: a token made for it, a TLS or WebRTC connection pinned to
// the server, an auth frame, and the roster. A device that has an identity keeps it, one that has
// none gets the name typed at pairing, and either way the credential is the token's.
func TestPairedDeviceSignsIn(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.settings.AddToken("fish's phone", "tok-phone", ""); err != nil {
		t.Fatal(err)
	}
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings, deps: deps}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer rc.Stop()
	if rc.rtc == nil || rc.RTCFingerprint() == "" {
		t.Fatal("WebRTC listener didn't start, so the QR code has nothing to pin")
	}

	transports := map[string]func() net.Conn{
		"tls": func() net.Conn { return dialTLS(t, rc.ln.Addr().String()) },
		"webrtc": func() net.Conn {
			return rtcTestClient(t, rc.rtc.Addr().(*net.UDPAddr), rc.RTCFingerprint())
		},
	}
	for name, dial := range transports {
		t.Run(name, func(t *testing.T) {
			c := dial()
			me, refused := signIn(t, c, protocol.AuthData{Token: "tok-phone", DeviceType: "web"})
			c.Close()
			if refused != "" || me.Name != "fish's phone" || me.Credential != "fish's phone" {
				t.Fatalf("new device: %+v refused=%q, want the pairing name and its credential", me, refused)
			}

			c = dial()
			me, refused = signIn(t, c, protocol.AuthData{Token: "tok-phone", DeviceType: "web", Name: "fish", Avatar: "data:image/png;base64,AAA"})
			c.Close()
			if refused != "" || me.Name != "fish" || me.Avatar != "data:image/png;base64,AAA" || me.Credential != "fish's phone" || me.DeviceType != "web" {
				t.Fatalf("device with an identity: %+v refused=%q, want it kept, with the token as credential", me, refused)
			}

			c = dial()
			_, refused = signIn(t, c, protocol.AuthData{Token: "wrong"})
			c.Close()
			if refused == "" {
				t.Fatal("a wrong token was let in")
			}
		})
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func startPairingServer(t *testing.T) (connDeps, *remoteControl) {
	t.Helper()
	deps := newTestDeps(t)
	deps.pairings = newPairingGate()
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings, deps: deps}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rc.Stop)
	return deps, rc
}

// A pairing token is for one phone: it works once for the first device that shows up, only that
// device after, and not at all once it has expired.
func TestPairingTokenIsOneDevice(t *testing.T) {
	deps, rc := startPairingServer(t)
	dial := func() net.Conn { return dialTLS(t, rc.ln.Addr().String()) }
	use := func(token, device string) string {
		c := dial()
		defer c.Close()
		_, refused := signIn(t, c, protocol.AuthData{Token: token, DeviceID: device})
		return refused
	}

	if err := deps.settings.AddPairingToken("phone", "tok-1", ""); err != nil {
		t.Fatal(err)
	}
	if r := use("tok-1", "dev-A"); r != "" {
		t.Fatalf("first device refused: %s", r)
	}
	if r := use("tok-1", "dev-A"); r != "" {
		t.Fatalf("same device refused on reconnect: %s", r)
	}
	if r := use("tok-1", "dev-B"); !strings.Contains(r, "already used") {
		t.Fatalf("second device got %q, want it refused as already used", r)
	}

	if err := deps.settings.AddPairingToken("late", "tok-2", ""); err != nil {
		t.Fatal(err)
	}
	deps.settings.mu.Lock()
	rec := deps.settings.Tokens["late"]
	rec.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	deps.settings.Tokens["late"] = rec
	deps.settings.mu.Unlock()
	if r := use("tok-2", "dev-A"); !strings.Contains(r, "expired") {
		t.Fatalf("expired token got %q", r)
	}
	if _, _, ok := deps.settings.LookupToken("tok-2"); ok {
		t.Fatal("an expired pairing token was left behind")
	}

	if err := deps.settings.AddPairingToken("nodev", "tok-3", ""); err != nil {
		t.Fatal(err)
	}
	if r := use("tok-3", ""); !strings.Contains(r, "too old") {
		t.Fatalf("a client with no device id got %q", r)
	}
}

// With a desktop connected, a new device waits until it says yes; no, or silence, refuses it.
func TestPairingNeedsApprovalWhenSomeoneCanGiveIt(t *testing.T) {
	deps, rc := startPairingServer(t)
	local := &syncBuf{}
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), local), &fakeCloser{}, "unix", peerIdentity{Name: "desktop", DeviceType: "desktop"})

	try := func(name string, decide func(), wait time.Duration) string {
		if err := deps.settings.AddPairingToken(name, "tok-"+name, ""); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if decide == nil {
				return
			}
			for i := 0; i < 200 && !deps.pairings.answerable(name); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			decide()
		}()
		pairingApprovalWait = wait
		c := dialTLS(t, rc.ln.Addr().String())
		defer c.Close()
		_, refused := signIn(t, c, protocol.AuthData{Token: "tok-" + name, DeviceID: "dev", Name: "fish"})
		<-done
		return refused
	}
	t.Cleanup(func() { pairingApprovalWait = 60 * time.Second })

	if r := try("yes", func() { deps.pairings.answer("yes", true) }, 5*time.Second); r != "" {
		t.Fatalf("approved device refused: %s", r)
	}
	if !strings.Contains(local.String(), `"pairing_request"`) || !strings.Contains(local.String(), `"credential":"yes"`) {
		t.Fatalf("the desktop was never asked: %s", local.String())
	}
	if r := try("no", func() { deps.pairings.answer("no", false) }, 5*time.Second); !strings.Contains(r, "declined") {
		t.Fatalf("declined device got %q", r)
	}
	if _, _, ok := deps.settings.LookupToken("tok-no"); ok {
		t.Fatal("a declined pairing token survived")
	}
	if r := try("silent", nil, 200*time.Millisecond); !strings.Contains(r, "in time") {
		t.Fatalf("unanswered pairing got %q", r)
	}
}
