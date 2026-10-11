package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/cpace"
	"yoakeland/internal/protocol"
)

// These run the desktop app's real pairing client (yoake-electron/electron/pair.js, under node)
// against this server, so the two implementations are checked against each other and not only
// against the draft's vectors. They are skipped when node or the app's sources aren't next to this tree.

type jsPairResult struct {
	Token   string `json:"token"`
	CAPem   string `json:"caPem"`
	Error   string `json:"error"`
	Waiting bool   `json:"waiting"`
}

func jsPair(t *testing.T, host string, port int, pin, deviceID string) jsPairResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	pairJS, err := filepath.Abs("../../../yoake-electron/electron/pair.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pairJS); err != nil {
		t.Skip("the desktop app's sources are not next to this tree")
	}
	script := `
const { pairWithPin } = await import(process.env.PAIR_JS);
let waiting = false;
try {
  const r = await pairWithPin(JSON.parse(process.env.PAIR_ARGS), { onWaiting: () => (waiting = true) });
  console.log(JSON.stringify({ ...r, waiting }));
} catch (e) {
  console.log(JSON.stringify({ error: e.message, waiting }));
}`
	args, _ := json.Marshal(map[string]any{"host": host, "port": port, "pin": pin, "deviceId": deviceID, "name": "fish"})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script)
	cmd.Env = append(os.Environ(), "PAIR_JS=file://"+pairJS, "PAIR_ARGS="+string(args))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var res jsPairResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		t.Fatalf("node printed %q: %v\n%s", out, err, stderr.String())
	}
	return res
}

func hostPort(t *testing.T, addr net.Addr) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	return h, port
}

func TestDesktopClientPairsWithTheServer(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	host, port := hostPort(t, rc.ln.Addr())

	got := jsPair(t, host, port, formatPIN(pin), "desktop-1")
	if got.Error != "" || got.Token == "" {
		t.Fatalf("pairing failed: %+v", got)
	}
	wantCA, _ := rc.CAPEM()
	if wantCA == "" || got.CAPem != wantCA {
		t.Fatalf("the client was handed a different CA than the server's:\n got %q\nwant %q", got.CAPem, wantCA)
	}
	c := dialTLS(t, rc.ln.Addr().String())
	me, refused := signIn(t, c, protocol.AuthData{Token: got.Token, DeviceID: "desktop-1", DeviceType: "desktop"})
	c.Close()
	if refused != "" || !strings.HasPrefix(me.Credential, "pin-") {
		t.Fatalf("the credential did not sign in: %+v %q", me, refused)
	}
	if pinCredentials(deps) != 1 {
		t.Fatal("expected exactly one credential")
	}

	// Wrong PIN, and the spent one again.
	if bad := jsPair(t, host, port, "00000000", "desktop-2"); bad.Token != "" || bad.Error == "" {
		t.Fatalf("a wrong PIN paired: %+v", bad)
	}
	if again := jsPair(t, host, port, pin, "desktop-3"); again.Token != "" || again.Error == "" {
		t.Fatalf("a spent PIN paired: %+v", again)
	}
}

func TestDesktopClientIsNotFooledByARelay(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	relay := startTLSRelay(t, rc.ln.Addr().String())
	host, port := hostPort(t, relay.Addr())
	if got := jsPair(t, host, port, pin, "desktop-1"); got.Token != "" || got.Error == "" {
		t.Fatalf("paired through a relay that terminates TLS: %+v", got)
	}
	if n := pinCredentials(deps); n != 0 {
		t.Fatalf("%d credentials made through the relay", n)
	}
}

// A server that does not know the PIN cannot make the client believe it does.
func TestDesktopClientRejectsAnImpostorServer(t *testing.T) {
	ln := selfSignedListener(t)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var env protocol.Envelope
				var auth protocol.AuthData
				if json.Unmarshal(line, &env) != nil || json.Unmarshal(env.Data, &auth) != nil || auth.Pake == nil {
					return
				}
				sid, ya, _ := decodePakeStart(auth.Pake)
				state := c.(*tls.Conn).ConnectionState()
				binding, _ := state.ExportKeyingMaterial(pakeExporter, []byte(pakeLabel), 32)
				// It plays the protocol faithfully, with a guess at the PIN.
				sess, _ := cpace.Start([]byte("12345678"), pakeCI(binding), sid, nil)
				isk, _ := sess.FinishResponder(ya, []byte(auth.DeviceID))
				send := func(typ protocol.EventType, data any) {
					b, _ := json.Marshal(protocol.Envelope{Type: typ, Data: mustJSON(t, data)})
					_, _ = c.Write(append(b, '\n'))
				}
				send(protocol.EventPakeReply, protocol.PakeReplyData{Yb: base64.StdEncoding.EncodeToString(sess.Message())})
				if _, err := r.ReadBytes('\n'); err != nil { // the client's confirmation, which it cannot check
					return
				}
				confirmKey, sealKey, _ := pakeKeys(isk)
				_ = confirmKey
				sealed, _ := pakeSeal(sealKey, []byte(`{"token":"stolen","caPem":"evil"}`))
				send(protocol.EventPakeResult, protocol.PakeResultData{Sealed: base64.StdEncoding.EncodeToString(sealed)})
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	host, port := hostPort(t, ln.Addr())
	got := jsPair(t, host, port, "87654321", "desktop-1")
	if got.Token != "" || !strings.Contains(got.Error, "doesn't know the PIN") {
		t.Fatalf("the client accepted an impostor: %+v", got)
	}
}

func TestDesktopClientWaitsForApproval(t *testing.T) {
	deps, rc, pin := startPINServer(t)
	local := &syncBuf{}
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), local), &fakeCloser{}, "unix", peerIdentity{Name: "desktop", DeviceType: "desktop"})
	rc.deps = deps
	pairingApprovalWait = 20 * time.Second
	t.Cleanup(func() { pairingApprovalWait = 60 * time.Second })
	go func() {
		for i := 0; i < 2000; i++ {
			deps.pairings.mu.Lock()
			var name string
			for n := range deps.pairings.waiting {
				name = n
			}
			deps.pairings.mu.Unlock()
			if name != "" {
				time.Sleep(300 * time.Millisecond) // long enough for the client to be told it is waiting
				deps.pairings.answer(name, true)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	host, port := hostPort(t, rc.ln.Addr())
	got := jsPair(t, host, port, pin, "desktop-1")
	if got.Token == "" || !got.Waiting {
		t.Fatalf("an approved device: %+v (it should have been told to wait)", got)
	}
}

func selfSignedListener(t *testing.T) net.Listener {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mitm"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startTLSRelay is a man in the middle with its own certificate that forwards every byte untouched.
func startTLSRelay(t *testing.T, upstream string) net.Listener {
	t.Helper()
	ln := selfSignedListener(t)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			down, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := tls.Dial("tcp", upstream, &tls.Config{InsecureSkipVerify: true})
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
	return ln
}
