package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
)

// rtcTestClient does what the browser client does: picks a ufrag, builds the server's answer from
// its address and fingerprint, and opens the negotiated data channel.
func rtcTestClient(t *testing.T, addr *net.UDPAddr, fingerprint string) net.Conn {
	t.Helper()
	raw := make([]byte, 18)
	_, _ = rand.Read(raw)
	ufrag := rtcUfragPrefix + base64.RawURLEncoding.EncodeToString(raw)

	se := webrtc.SettingEngine{}
	se.SetICECredentials(ufrag, ufrag)
	se.DetachDataChannels()
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	dc, err := pc.CreateDataChannel("", &webrtc.DataChannelInit{Negotiated: ptr(true), ID: ptr(uint16(0))})
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan net.Conn, 1)
	dc.OnOpen(func() {
		rw, err := dc.DetachWithDeadline()
		if err != nil {
			t.Error(err)
			return
		}
		opened <- newRTCConn(rw, func() {}, addr)
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	ip := addr.IP.String()
	answer := strings.Join([]string{
		"v=0", "o=- 0 0 IN IP4 " + ip, "s=-", "t=0 0", "a=ice-lite", "a=group:BUNDLE 0",
		fmt.Sprintf("m=application %d UDP/DTLS/SCTP webrtc-datachannel", addr.Port),
		"c=IN IP4 " + ip, "a=mid:0", "a=ice-options:ice2",
		"a=ice-ufrag:" + ufrag, "a=ice-pwd:" + ufrag,
		"a=fingerprint:sha-256 " + fingerprint,
		"a=setup:passive", "a=sctp-port:5000", fmt.Sprintf("a=max-message-size:%d", rtcMaxMessage),
		fmt.Sprintf("a=candidate:1467250680 1 UDP 1467250680 %s %d typ host", ip, addr.Port),
		"a=end-of-candidates", "",
	}, "\r\n")
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-opened:
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("data channel never opened")
		return nil
	}
}

func startTestRTC(t *testing.T, accept func(net.Conn)) (*net.UDPAddr, string) {
	t.Helper()
	cert, fp, err := loadRTCCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l, err := startRTCListener(ctx, "127.0.0.1:0", cert, accept)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); cancel() })
	return l.Addr().(*net.UDPAddr), fp
}

// Lines of any size survive the 16 KiB message limit in both directions.
func TestRTCEchoesLines(t *testing.T) {
	addr, fp := startTestRTC(t, func(c net.Conn) {
		defer c.Close()
		r := bufio.NewReaderSize(c, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if _, err := c.Write([]byte(line)); err != nil {
				return
			}
		}
	})
	c := rtcTestClient(t, addr, fp)
	defer c.Close()
	r := bufio.NewReaderSize(c, 1<<20)
	for _, size := range []int{5, 100, rtcMaxMessage - 1, rtcMaxMessage + 1, 200_000} {
		line := strings.Repeat("x", size) + "\n"
		if _, err := c.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, err := r.ReadString('\n')
		if err != nil || got != line {
			t.Fatalf("size %d: got %d bytes, err %v", size, len(got), err)
		}
	}
}

// A packet that isn't a yoake STUN check starts nothing, and a client that goes away is cleaned up.
func TestRTCIgnoresNoiseAndCleansUp(t *testing.T) {
	accepted := make(chan net.Conn, 1)
	addr, fp := startTestRTC(t, func(c net.Conn) { accepted <- c })
	junk, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer junk.Close()
	_, _ = junk.Write([]byte("hello"))
	select {
	case <-accepted:
		t.Fatal("noise opened a connection")
	case <-time.After(300 * time.Millisecond):
	}

	c := rtcTestClient(t, addr, fp)
	srv := <-accepted
	_ = c.Close()
	_ = srv.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := srv.Read(make([]byte, 1)); err == nil {
		t.Fatal("server side never saw the close")
	}
}

func TestRTCCertIsStable(t *testing.T) {
	dir := t.TempDir()
	_, a, err := loadRTCCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := loadRTCCert(dir)
	if err != nil || a != b {
		t.Fatalf("fingerprint changed across loads: %q vs %q (%v)", a, b, err)
	}
}

// Close frees the UDP port before it returns: a listener restarted on the same address binds.
func TestRTCCloseFreesPort(t *testing.T) {
	cert, _, err := loadRTCCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, err := startRTCListener(context.Background(), "127.0.0.1:0", cert, func(net.Conn) {})
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	l2, err := startRTCListener(context.Background(), addr, cert, func(net.Conn) {})
	if err != nil {
		t.Fatalf("port still held after Close: %v", err)
	}
	l2.Close()
}

// One source address can start at most rtcMaxPeersPerSource setups at once; a different one still can.
func TestRTCPerSourceLimit(t *testing.T) {
	cert, _, err := loadRTCCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, err := startRTCListener(context.Background(), "127.0.0.1:0", cert, func(net.Conn) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	knock := func(i int) []byte {
		u := fmt.Sprintf("%s%016d:%s", rtcUfragPrefix, i, fmt.Sprintf("%016d", i))
		m := stun.New()
		if err := m.Build(stun.BindingRequest, stun.NewUsername(u)); err != nil {
			t.Fatal(err)
		}
		return m.Raw
	}
	src := netip.MustParseAddrPort("203.0.113.7:4000")
	for i := 0; i < rtcMaxPeersPerSource+3; i++ {
		l.onUnhandled(knock(i), src)
	}
	l.onUnhandled(knock(100), netip.MustParseAddrPort("203.0.113.8:4000"))
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.active); n != rtcMaxPeersPerSource+1 {
		t.Fatalf("%d setups started, want %d (8 from one source, 1 from another)", n, rtcMaxPeersPerSource+1)
	}
}
