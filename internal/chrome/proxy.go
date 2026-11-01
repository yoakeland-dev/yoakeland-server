package chrome

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// SOCKS5 reply codes (RFC 1928) used here.
const (
	repOK          = 0
	repNotAllowed  = 2
	repHostUnreach = 4
	repRefused     = 5
	repBadCommand  = 7
	repBadAddrType = 8
)

const handshakeTimeout = 30 * time.Second

// proxy is a minimal SOCKS5 server (CONNECT, no auth) on 127.0.0.1. It
// resolves names itself, refuses the connection if any resolved address is
// Blocked, and dials only the addresses it checked, so DNS rebinding can't
// swap in a different one. Chrome is forced through it with
// --proxy-bypass-list=<-loopback>, which also covers redirects, workers,
// WebSockets and prefetch.
type proxy struct {
	ln       net.Listener
	ownPorts []uint16
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newProxy(ownPorts []uint16) (*proxy, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{ln: ln, ownPorts: ownPorts, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) close() {
	p.ln.Close()
	p.mu.Lock()
	conns := p.conns
	p.conns = nil // track refuses new connections from now on
	p.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// track registers c, or closes it if the proxy is already shut down.
func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond) // e.g. EMFILE: back off, keep serving
			continue
		}
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() || !p.track(c) {
			c.Close()
			continue
		}
		go p.handle(c)
	}
}

func (p *proxy) handle(c net.Conn) {
	defer p.untrack(c)
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(c)
	host, port, code := readRequest(br, c)
	if code != repOK {
		reply(c, code)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	up, code := p.connect(ctx, host, port)
	if code != repOK {
		reply(c, code)
		return
	}
	if !p.track(up) {
		return
	}
	defer p.untrack(up)
	reply(c, repOK)
	_ = c.SetDeadline(time.Time{})

	// br may hold bytes the client sent before our reply.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, br)
		_ = up.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(c, up)
	_ = c.(*net.TCPConn).CloseWrite()
	<-done
}

func (p *proxy) connect(ctx context.Context, host string, port uint16) (net.Conn, byte) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil || len(ips) == 0 {
		return nil, repHostUnreach
	}
	local := localAddrs()
	for _, ip := range ips {
		if Blocked(ip, port, p.ownPorts, local) {
			return nil, repNotAllowed
		}
	}
	var d net.Dialer
	for _, ip := range ips {
		if c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(canonical(ip), port).String()); err == nil {
			return c, repOK
		}
	}
	return nil, repRefused
}

// readRequest performs the method negotiation and reads a CONNECT request.
func readRequest(br *bufio.Reader, w io.Writer) (host string, port uint16, code byte) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || hdr[0] != 5 {
		return "", 0, repBadCommand
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return "", 0, repBadCommand
	}
	if !bytes.Contains(methods, []byte{0}) {
		_, _ = w.Write([]byte{5, 0xff})
		return "", 0, repBadCommand
	}
	if _, err := w.Write([]byte{5, 0}); err != nil {
		return "", 0, repBadCommand
	}
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != 5 || req[1] != 1 {
		return "", 0, repBadCommand
	}
	var addr []byte
	switch req[3] {
	case 1:
		addr = make([]byte, 4)
	case 4:
		addr = make([]byte, 16)
	case 3:
		n, err := br.ReadByte()
		if err != nil || n == 0 {
			return "", 0, repBadCommand
		}
		addr = make([]byte, n)
	default:
		return "", 0, repBadAddrType
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, addr); err != nil {
		return "", 0, repBadCommand
	}
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return "", 0, repBadCommand
	}
	port = uint16(pb[0])<<8 | uint16(pb[1])
	if port == 0 {
		return "", 0, repNotAllowed
	}
	if req[3] == 3 {
		return string(addr), port, repOK
	}
	ip, _ := netip.AddrFromSlice(addr)
	return ip.String(), port, repOK
}

func reply(w io.Writer, code byte) {
	_, _ = w.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}
