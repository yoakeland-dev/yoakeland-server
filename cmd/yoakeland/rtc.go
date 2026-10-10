package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
)

// WebRTC clients (SPEC.md §2a "WebRTC clients"). A browser on any HTTPS origin, app.yoake.land
// included, can't open a WebSocket to a server whose certificate its trust store doesn't know,
// and a pairing code can't carry a CA into one. A WebRTC data channel verifies the peer by the
// DTLS certificate *fingerprint* instead, so the pairing code carries that and nothing needs
// installing. The shape follows libp2p's "webrtc-direct" (no signaling server):
//
//   - The server listens on one UDP port, ICE-lite, with a fixed certificate (stored, so its
//     fingerprint stays valid across restarts).
//   - The client builds the server's SDP answer itself from the pairing code (address, port,
//     fingerprint) and picks its own ICE ufrag, used as both ufrag and password.
//   - The server learns that ufrag from the first STUN packet (onUnhandled), answers a synthetic
//     offer built from it, and a negotiated data channel (id 0) opens.
//   - The data channel then carries exactly the TCP protocol: an auth frame first, then
//     newline-delimited JSON. The client is authenticated by the token, not by DTLS.

const (
	// rtcUfragPrefix filters random UDP noise: only a client that knows this scheme starts a peer.
	rtcUfragPrefix = "yoake+rtc+v1/"
	// rtcMaxMessage is the largest data-channel message either side sends (the SDP advertises it);
	// 16 KiB is what every browser interoperates at.
	rtcMaxMessage = 16384
	// rtcMaxPeers bounds peers in setup plus live ones, so a UDP flood can't allocate without end.
	rtcMaxPeers = 64
	// rtcSetupTimeout is how long a peer may take to open its data channel.
	rtcSetupTimeout = 20 * time.Second
)

// rtcCertValidity is long: a browser doesn't check a DTLS certificate's dates, and the fingerprint
// is in every pairing code, so rotating it would unpair everyone.
const rtcCertValidity = 20 * 365 * 24 * time.Hour

// loadRTCCert returns the server's DTLS certificate, creating and storing it on first use, and its
// SDP-style fingerprint (uppercase colon-separated SHA-256).
func loadRTCCert(stateDir string) (webrtc.Certificate, string, error) {
	path := filepath.Join(stateDir, "tls", "rtc.pem")
	if raw, err := os.ReadFile(path); err == nil {
		var certDER, keyDER []byte
		for rest := raw; ; {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			switch b.Type {
			case "CERTIFICATE":
				certDER = b.Bytes
			case "EC PRIVATE KEY":
				keyDER = b.Bytes
			}
		}
		if certDER != nil && keyDER != nil {
			if key, err := x509.ParseECPrivateKey(keyDER); err == nil {
				if x, err := x509.ParseCertificate(certDER); err == nil {
					return newRTCCert(key, x, fingerprintOf(certDER))
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return webrtc.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return webrtc.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "yoakeland-webrtc"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(rtcCertValidity),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return webrtc.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return webrtc.Certificate{}, "", err
	}
	out := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return webrtc.Certificate{}, "", err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return webrtc.Certificate{}, "", err
	}
	x, err := x509.ParseCertificate(der)
	if err != nil {
		return webrtc.Certificate{}, "", err
	}
	return newRTCCert(key, x, fingerprintOf(der))
}

func newRTCCert(key *ecdsa.PrivateKey, x *x509.Certificate, fp string) (webrtc.Certificate, string, error) {
	// Not NewCertificate, which signs a fresh certificate: the stored one's fingerprint is the one
	// in every pairing code.
	return webrtc.CertificateFromX509(key, x), fp, nil
}

func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// rtcListener owns the UDP port and starts a peer for every client that knocks with a valid ufrag.
type rtcListener struct {
	ctx    context.Context
	cancel context.CancelFunc
	udp    net.PacketConn
	mux    *ice.UDPMuxDefault
	cert   webrtc.Certificate
	accept func(net.Conn)

	mu     sync.Mutex
	active map[string]struct{} // ufrags in setup or live
}

// startRTCListener binds addr (UDP) and calls accept for each opened data channel, wrapped as a
// net.Conn that speaks the same protocol as a TCP connection. Close stops it and every peer.
func startRTCListener(ctx context.Context, addr string, cert webrtc.Certificate, accept func(net.Conn)) (*rtcListener, error) {
	udp, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &rtcListener{ctx: ctx, cancel: cancel, udp: udp, cert: cert, accept: accept, active: map[string]struct{}{}}
	// The muxer calls onUnhandled from its own goroutine as soon as it exists, so l.mux is only
	// visible to it once assigned.
	l.mu.Lock()
	l.mux = ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: udp, OnUnhandledPacket: l.onUnhandled})
	l.mu.Unlock()
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		mux := l.mux
		l.mu.Unlock()
		_ = mux.Close()
		_ = udp.Close()
	}()
	return l, nil
}

func (l *rtcListener) Addr() net.Addr { return l.udp.LocalAddr() }

func (l *rtcListener) Close() { l.cancel() }

// onUnhandled sees every UDP packet the muxer can't route to a known ufrag. A STUN binding request
// whose username is "<ufrag>:<ufrag>" with our prefix is a new client; its first packet is dropped
// (the browser's connectivity checks retry within milliseconds).
func (l *rtcListener) onUnhandled(data []byte, from netip.AddrPort) {
	if !stun.IsMessage(data) {
		return
	}
	msg := &stun.Message{Raw: append([]byte(nil), data...)}
	if msg.Decode() != nil {
		return
	}
	user, err := msg.Get(stun.AttrUsername)
	if err != nil {
		return
	}
	ufrag, _, _ := strings.Cut(string(user), ":")
	if !strings.HasPrefix(ufrag, rtcUfragPrefix) || len(ufrag) < len(rtcUfragPrefix)+16 || len(ufrag) > 128 {
		return
	}
	l.mu.Lock()
	if _, dup := l.active[ufrag]; dup || len(l.active) >= rtcMaxPeers {
		l.mu.Unlock()
		return
	}
	l.active[ufrag] = struct{}{}
	l.mu.Unlock()
	go l.runPeer(ufrag, from)
}

func (l *rtcListener) runPeer(ufrag string, from netip.AddrPort) {
	defer func() {
		l.mu.Lock()
		delete(l.active, ufrag)
		l.mu.Unlock()
	}()
	pc, err := l.newPeerConnection(ufrag)
	if err != nil {
		logWarn("webrtc: peer setup for %s failed: %v", from, err)
		return
	}
	closed := make(chan struct{})
	var closeOnce sync.Once
	closePeer := func() {
		closeOnce.Do(func() {
			close(closed)
			_ = pc.Close()
		})
	}
	opened := make(chan struct{})
	dc, err := pc.CreateDataChannel("", &webrtc.DataChannelInit{Negotiated: ptr(true), ID: ptr(uint16(0))})
	if err != nil {
		closePeer()
		return
	}
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			closePeer()
			return
		}
		close(opened)
		l.accept(newRTCConn(raw, closePeer, net.UDPAddrFromAddrPort(from)))
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			closePeer()
		}
	})
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: rtcClientOffer(ufrag)}
	if err := pc.SetRemoteDescription(offer); err != nil {
		logWarn("webrtc: bad offer from %s: %v", from, err)
		closePeer()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(answer)
	}
	if err != nil {
		logWarn("webrtc: answer for %s failed: %v", from, err)
		closePeer()
		return
	}
	select {
	case <-opened:
	case <-time.After(rtcSetupTimeout):
		closePeer()
		return
	case <-l.ctx.Done():
		closePeer()
		return
	}
	select {
	case <-closed:
	case <-l.ctx.Done():
		closePeer()
	}
}

func (l *rtcListener) newPeerConnection(ufrag string) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	l.mu.Lock()
	mux := l.mux
	l.mu.Unlock()
	se.SetICEUDPMux(mux)
	se.SetICECredentials(ufrag, ufrag)
	// The client's certificate is whatever its browser made up; the token is what authenticates it.
	se.DisableCertificateFingerprintVerification(true)
	se.SetAnsweringDTLSRole(webrtc.DTLSRoleServer)
	se.DetachDataChannels()
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	return api.NewPeerConnection(webrtc.Configuration{Certificates: []webrtc.Certificate{l.cert}})
}

// rtcClientOffer is the offer the server pretends the client sent: the client's ICE credentials
// are its ufrag, and its certificate is a placeholder the server doesn't verify.
func rtcClientOffer(ufrag string) string {
	return strings.Join([]string{
		"v=0",
		"o=- 0 0 IN IP4 0.0.0.0",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=mid:0",
		"a=ice-options:trickle",
		"a=ice-ufrag:" + ufrag,
		"a=ice-pwd:" + ufrag,
		"a=fingerprint:sha-256 " + strings.TrimSuffix(strings.Repeat("FF:", 32), ":"),
		"a=setup:actpass",
		"a=sctp-port:5000",
		fmt.Sprintf("a=max-message-size:%d", rtcMaxMessage),
		"",
	}, "\r\n")
}

func ptr[T any](v T) *T { return &v }

// rtcConn adapts a detached data channel to the net.Conn serveAccepted wants. Reads go through a
// net.Pipe so deadlines and EOF behave like a socket's; writes are cut into data-channel messages.
type rtcConn struct {
	net.Conn // the pipe's read side
	rw       io.ReadWriteCloser
	remote   net.Addr
	closer   func()
}

func newRTCConn(rw io.ReadWriteCloser, closePeer func(), remote net.Addr) *rtcConn {
	feed, read := net.Pipe()
	c := &rtcConn{Conn: read, rw: rw, remote: remote, closer: closePeer}
	go func() {
		defer c.Close()
		buf := make([]byte, 64*1024)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				if _, werr := feed.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = feed.Close() // the reader sees EOF
				return
			}
		}
	}()
	return c
}

func (c *rtcConn) Write(p []byte) (int, error) {
	for sent := 0; sent < len(p); {
		end := min(sent+rtcMaxMessage, len(p))
		if _, err := c.rw.Write(p[sent:end]); err != nil {
			return sent, err
		}
		sent = end
	}
	return len(p), nil
}

func (c *rtcConn) RemoteAddr() net.Addr { return c.remote }

func (c *rtcConn) Close() error {
	err := c.Conn.Close()
	_ = c.rw.Close()
	c.closer()
	return err
}
