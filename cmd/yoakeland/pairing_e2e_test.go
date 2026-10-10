package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
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
