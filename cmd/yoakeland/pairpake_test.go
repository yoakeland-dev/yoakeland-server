package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/cpace"
	"yoakeland/internal/protocol"
)

// startPINServer is a real remote listener (TLS and WebRTC) with a PIN to pair with.
func startPINServer(t *testing.T) (connDeps, *remoteControl, string) {
	t.Helper()
	authFailDelay = 0
	t.Cleanup(func() { authFailDelay = time.Second })
	deps := newTestDeps(t)
	deps.pairings = newPairingGate()
	deps.pins = newPinRegistry()
	rc := &remoteControl{addr: "127.0.0.1:0", stateDir: t.TempDir(), settings: deps.settings}
	deps.remote = rc
	rc.deps = deps
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rc.Stop)
	return deps, rc, deps.pins.Mint()
}

// pakeAttempt is a client written against the wire format in SPEC.md, independent of the server's
// code: what a device does with a PIN it was told.
type pakeAttempt struct {
	pin, deviceID string
	noBinding     bool          // claim no channel binding although the transport has one
	tamper        func(*[]byte) // alter the confirmation MAC before it is sent
	badShare      []byte        // send this instead of a real share
	events        []protocol.EventType
}

type pakeOutcome struct {
	token, caPem string
	refused      string
}

func (a *pakeAttempt) run(t *testing.T, conn net.Conn) pakeOutcome {
	t.Helper()
	var binding []byte
	if tc, ok := conn.(*tls.Conn); ok && !a.noBinding {
		state := tc.ConnectionState()
		var err error
		if binding, err = state.ExportKeyingMaterial("EXPORTER-yoake-pair-v1", []byte("yoake/pair/v1"), 32); err != nil {
			t.Fatal(err)
		}
	}
	sid := make([]byte, 16)
	_, _ = rand.Read(sid)
	sess, err := cpace.Start([]byte(normalizePIN(a.pin)), cpace.LVCat([]byte("yoake/pair/v1"), binding), sid, []byte(a.deviceID))
	if err != nil {
		t.Fatal(err)
	}
	ya := sess.Message()
	if a.badShare != nil {
		ya = a.badShare
	}
	send := func(typ protocol.EventType, data any) {
		b, _ := json.Marshal(protocol.Envelope{Type: typ, Data: mustJSON(t, data)})
		if _, err := conn.Write(append(b, '\n')); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	next := func() (protocol.Envelope, bool) {
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return protocol.Envelope{}, false
			}
			var env protocol.Envelope
			if json.Unmarshal(line, &env) != nil {
				continue
			}
			a.events = append(a.events, env.Type)
			if env.Type == protocol.EventPairingPending {
				continue
			}
			return env, true
		}
	}
	stage := "the server's share"
	refusal := func(env protocol.Envelope, ok bool) string {
		if !ok {
			return "connection closed while waiting for " + stage
		}
		var e protocol.ErrorData
		_ = json.Unmarshal(env.Data, &e)
		return e.Message
	}

	send(protocol.EventAuth, protocol.AuthData{Name: "fish", DeviceType: "desktop", DeviceID: a.deviceID,
		Pake: &protocol.PakeStart{SID: base64.StdEncoding.EncodeToString(sid), Ya: base64.StdEncoding.EncodeToString(ya)}})
	env, ok := next()
	if !ok || env.Type != protocol.EventPakeReply {
		return pakeOutcome{refused: refusal(env, ok)}
	}
	var reply protocol.PakeReplyData
	_ = json.Unmarshal(env.Data, &reply)
	yb, _ := base64.StdEncoding.DecodeString(reply.Yb)
	isk, err := sess.FinishInitiator(yb, nil)
	if err != nil {
		return pakeOutcome{refused: "aborted: " + err.Error()}
	}
	stage = "the credential"
	confirmKey, _ := hkdf.Key(sha512.New, isk, nil, "yoake/pair/v1 confirm", 32)
	sealKey, _ := hkdf.Key(sha512.New, isk, nil, "yoake/pair/v1 seal", 32)
	mac := pakeMac(confirmKey, sid)
	if a.tamper != nil {
		a.tamper(&mac)
	}
	send(protocol.EventPakeConfirm, protocol.PakeConfirmData{Mac: base64.StdEncoding.EncodeToString(mac)})
	env, ok = next()
	if !ok || env.Type != protocol.EventPakeResult {
		return pakeOutcome{refused: refusal(env, ok)}
	}
	var res protocol.PakeResultData
	_ = json.Unmarshal(env.Data, &res)
	sealed, _ := base64.StdEncoding.DecodeString(res.Sealed)
	block, _ := aes.NewCipher(sealKey)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return pakeOutcome{refused: "short result"}
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("yoake/pair/v1 result"))
	if err != nil {
		return pakeOutcome{refused: "result did not open: the server did not know the PIN"}
	}
	var out struct{ Token, CAPem string }
	_ = json.Unmarshal(plain, &out)
	return pakeOutcome{token: out.Token, caPem: out.CAPem}
}

// pinCredentials counts the credentials a PIN pairing made (the owner's own "default" token is not one).
func pinCredentials(deps connDeps) int {
	deps.settings.mu.Lock()
	defer deps.settings.mu.Unlock()
	n := 0
	for name := range deps.settings.Tokens {
		if strings.HasPrefix(name, "pin-") {
			n++
		}
	}
	return n
}

func pairOver(t *testing.T, rc *remoteControl, a *pakeAttempt) pakeOutcome {
	t.Helper()
	c := dialTLS(t, rc.ln.Addr().String())
	defer c.Close()
	return a.run(t, c)
}

func TestPINPairingEndToEnd(t *testing.T) {
	deps, rc, pin := startPINServer(t)

	got := pairOver(t, rc, &pakeAttempt{pin: formatPIN(pin), deviceID: "dev-A"}) // typed with its dash
	if got.refused != "" || got.token == "" {
		t.Fatalf("pairing failed: %+v", got)
	}
	if !strings.Contains(got.caPem, "BEGIN CERTIFICATE") {
		t.Errorf("the sealed result carried no CA to trust: %q", got.caPem)
	}

	// The credential is an ordinary one-device pairing token, already bound to this device.
	name, rec, ok := deps.settings.LookupToken(got.token)
	if !ok || !rec.Pairing || rec.DeviceID != "dev-A" || !strings.HasPrefix(name, "pin-") {
		t.Fatalf("token %q rec %+v", name, rec)
	}
	c := dialTLS(t, rc.ln.Addr().String())
	me, refused := signIn(t, c, protocol.AuthData{Token: got.token, DeviceID: "dev-A", DeviceType: "desktop"})
	c.Close()
	if refused != "" || me.Credential != name {
		t.Fatalf("signing in with it: %+v %q", me, refused)
	}
	c = dialTLS(t, rc.ln.Addr().String())
	_, refused = signIn(t, c, protocol.AuthData{Token: got.token, DeviceID: "dev-B"})
	c.Close()
	if !strings.Contains(refused, "already used") {
		t.Fatalf("another device with the same token got %q", refused)
	}

	// The PIN worked once.
	again := pairOver(t, rc, &pakeAttempt{pin: pin, deviceID: "dev-C"})
	if again.token != "" || again.refused == "" {
		t.Fatalf("a spent PIN paired a second device: %+v", again)
	}
}

func TestPINPairingOverWebRTCNeedsNoBinding(t *testing.T) {
	_, rc, pin := startPINServer(t)
	c := rtcTestClient(t, rc.rtc.Addr().(*net.UDPAddr), rc.RTCFingerprint())
	defer c.Close()
	got := (&pakeAttempt{pin: pin, deviceID: "dev-web"}).run(t, c)
	if got.refused != "" || got.token == "" {
		t.Fatalf("pairing over WebRTC failed: %+v", got)
	}
}

func TestWrongPINsRunOutAndAreReplaced(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	var replaced atomic.Value
	deps.pins.onExhausted = func(p string) { replaced.Store(p) }
	wrong := "00000000"
	if pin == wrong {
		wrong = "11111111"
	}
	for i := 0; i < pinAttempts; i++ {
		if got := pairOver(t, rc, &pakeAttempt{pin: wrong, deviceID: "dev"}); got.token != "" || got.refused == "" {
			t.Fatalf("attempt %d with a wrong PIN: %+v", i, got)
		}
	}
	// The right PIN is dead now: five guesses were spent on it.
	if got := pairOver(t, rc, &pakeAttempt{pin: pin, deviceID: "dev"}); got.token != "" {
		t.Fatalf("the correct PIN worked after its guesses ran out: %+v", got)
	}
	fresh, _ := replaced.Load().(string)
	if len(fresh) != pinDigits || fresh == pin {
		t.Fatalf("expected a replacement PIN to be announced, got %q", fresh)
	}
	if got := pairOver(t, rc, &pakeAttempt{pin: fresh, deviceID: "dev"}); got.refused != "" {
		t.Fatalf("the replacement PIN did not work: %+v", got)
	}
}

func TestSessionsRacingForOnePIN(t *testing.T) {
	_, rc, pin := startPINServer(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < pinAttempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if got := pairOver(t, rc, &pakeAttempt{pin: pin, deviceID: "dev-" + string(rune('a'+i))}); got.token != "" {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d devices paired with one single-use PIN", wins.Load())
	}
}

func TestExpiredPINIsRefused(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	deps.pins.now = func() time.Time { return time.Now().Add(PairingTTL + time.Second) }
	if got := pairOver(t, rc, &pakeAttempt{pin: pin, deviceID: "dev"}); got.token != "" {
		t.Fatalf("an expired PIN paired a device: %+v", got)
	}
}

func TestPINPairingRefusals(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	cases := map[string]*pakeAttempt{
		"wrong PIN":                  {pin: "99999999", deviceID: "d"},
		"tampered confirmation":      {pin: pin, deviceID: "d", tamper: func(m *[]byte) { (*m)[0] ^= 1 }},
		"all-zero (low-order) share": {pin: pin, deviceID: "d", badShare: make([]byte, 32)},
		"short share":                {pin: pin, deviceID: "d", badShare: make([]byte, 31)},
		"no channel binding on TLS":  {pin: pin, deviceID: "d", noBinding: true},
		"no device id":               {pin: pin, deviceID: ""},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			// Each needs a live PIN of its own, or one failure would use up the guesses of the next.
			pin := deps.pins.Mint()
			if a.pin != "99999999" {
				a.pin = pin
			}
			if got := pairOver(t, rc, a); got.token != "" || got.refused == "" {
				t.Fatalf("was let through: %+v", got)
			}
		})
	}
	if n := pinCredentials(deps); n != 0 {
		t.Fatalf("%d credentials were made by refused pairings", n)
	}
	_ = pin

	t.Run("no registry at all", func(t *testing.T) {
		deps.pins = nil
		rc.deps.pins = nil // pairings with nothing to pair with are refused, not crashed on
		if got := pairOver(t, rc, &pakeAttempt{pin: "12345678", deviceID: "d"}); got.token != "" {
			t.Fatalf("paired with no PIN registry: %+v", got)
		}
	})
}

// A garbled first frame must not get past the shape checks.
func TestPINPairingRejectsMalformedStarts(t *testing.T) {
	_, rc, _ := startPINServer(t)
	for name, p := range map[string]*protocol.PakeStart{
		"sid not base64": {SID: "!!!", Ya: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		"sid too short":  {SID: base64.StdEncoding.EncodeToString(make([]byte, 4)), Ya: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		"sid too long":   {SID: base64.StdEncoding.EncodeToString(make([]byte, 65)), Ya: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			c := dialTLS(t, rc.ln.Addr().String())
			defer c.Close()
			_, refused := signIn(t, c, protocol.AuthData{DeviceID: "d", Pake: p})
			if refused == "" {
				t.Fatal("accepted")
			}
		})
	}
}

// With a desktop connected, a PIN-paired device still waits for a yes, as a QR-paired one does.
func TestPINPairingAsksTheDesktop(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	local := &syncBuf{}
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), local), &fakeCloser{}, "unix", peerIdentity{Name: "desktop", DeviceType: "desktop"})
	rc.deps = deps
	t.Cleanup(func() { pairingApprovalWait = 60 * time.Second })
	pairingApprovalWait = 5 * time.Second

	answerNext := func(allow bool) {
		go func() {
			for i := 0; i < 500; i++ {
				deps.pairings.mu.Lock()
				var name string
				for n := range deps.pairings.waiting {
					name = n
				}
				deps.pairings.mu.Unlock()
				if name != "" {
					deps.pairings.answer(name, allow)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	answerNext(false)
	if got := pairOver(t, rc, &pakeAttempt{pin: pin, deviceID: "dev"}); got.token != "" || !strings.Contains(got.refused, "declined") {
		t.Fatalf("a declined device got %+v", got)
	}
	if !strings.Contains(local.String(), `"pairing_request"`) {
		t.Fatal("the desktop was never asked")
	}
	pin = deps.pins.Mint()
	answerNext(true)
	a := &pakeAttempt{pin: pin, deviceID: "dev"}
	if got := pairOver(t, rc, a); got.token == "" {
		t.Fatalf("an approved device was refused: %+v", got)
	}
	waited := false
	for _, e := range a.events {
		waited = waited || e == protocol.EventPairingPending
	}
	if !waited {
		t.Errorf("the device was never told it was waiting: %v", a.events)
	}
}

// A man in the middle that terminates TLS and relays every frame untouched still gets nowhere: its
// two TLS sessions have different exporters, so the key the client and the server derive differ.
func TestTLSMitmCannotRelayThePairing(t *testing.T) {
	deps, rc, pin := startPINServer(t)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mitm"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			down, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := tls.Dial("tcp", rc.ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
				if err != nil {
					down.Close()
					return
				}
				go func() { _, _ = io.Copy(up, down); up.Close() }()
				_, _ = io.Copy(down, up)
				down.Close()
			}()
		}
	}()

	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := (&pakeAttempt{pin: pin, deviceID: "dev"}).run(t, c); got.token != "" || got.refused == "" {
		t.Fatalf("a relay in the middle was let through: %+v", got)
	}
	if n := pinCredentials(deps); n != 0 {
		t.Fatalf("the relayed pairing made %d credentials", n)
	}

	// The same client straight to the server is fine: it was the relay that broke it.
	if got := pairOver(t, rc, &pakeAttempt{pin: deps.pins.Mint(), deviceID: "dev"}); got.refused != "" {
		t.Fatalf("control run failed: %+v", got)
	}
}
