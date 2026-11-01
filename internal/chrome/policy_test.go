package chrome

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"testing"
)

func TestBlocked(t *testing.T) {
	local := []netip.Addr{netip.MustParseAddr("192.168.1.10")}
	tests := []struct {
		ip      string
		port    uint16
		blocked bool
	}{
		{"169.254.169.254", 80, true},
		{"169.254.170.2", 80, true},
		{"100.100.100.200", 80, true},
		{"168.63.129.16", 80, true},
		{"fd00:ec2::254", 80, true},
		{"fe80::1", 80, true},
		{"fe80::1%eth0", 80, true},
		{"0.0.0.0", 80, true},
		{"0.1.2.3", 80, true},
		{"::", 80, true},
		{"224.0.0.1", 80, true},
		{"ff02::1", 80, true},
		{"255.255.255.255", 80, true},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", 80, true}, // Teredo
		// embeddings
		{"::ffff:169.254.169.254", 80, true},
		{"::ffff:a9fe:a9fe", 80, true},
		{"64:ff9b::a9fe:a9fe", 80, true},
		{"64:ff9b::7f00:1", 80, true},
		{"2002:a9fe:a9fe::1", 80, true},
		{"::169.254.169.254", 80, true},
		{"64:ff9b:1::1", 80, true},
		// allowed
		{"127.0.0.1", 80, false},
		{"::1", 80, false},
		{"::ffff:127.0.0.1", 80, false},
		{"10.0.0.5", 8080, false},
		{"192.168.1.10", 3000, false},
		{"93.184.216.34", 443, false},
		{"2606:4700::1111", 443, false},
		{"64:ff9b::5db8:d822", 443, false}, // NAT64 of a public address
		// own listen ports
		{"127.0.0.1", 7777, true},
		{"::1", 7777, true},
		{"::ffff:127.0.0.1", 7777, true},
		{"192.168.1.10", 7777, true},
		{"192.168.1.11", 7777, false},
		{"93.184.216.34", 7777, false},
	}
	for _, tc := range tests {
		got := Blocked(netip.MustParseAddr(tc.ip), tc.port, []uint16{7777}, local)
		if got != tc.blocked {
			t.Errorf("Blocked(%s, %d) = %v, want %v", tc.ip, tc.port, got, tc.blocked)
		}
	}
}

// socksConnect speaks just enough SOCKS5 to send a request by domain name,
// followed immediately by early payload bytes, and returns the reply code.
func socksConnect(t *testing.T, c net.Conn, cmd byte, host string, port int, early string) byte {
	t.Helper()
	req := []byte{5, 1, 0, 5, cmd, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	req = append(req, early...)
	if _, err := c.Write(req); err != nil { // greeting + request pipelined
		t.Fatal(err)
	}
	var rep [12]byte
	if _, err := io.ReadFull(c, rep[:2]); err != nil || rep[1] != 0 {
		t.Fatalf("method reply %v, %v", rep[:2], err)
	}
	if _, err := io.ReadFull(c, rep[:10]); err != nil {
		t.Fatal(err)
	}
	return rep[1]
}

func TestProxy(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { // upstream: reads a line, answers "echo <line>"
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = c.Write([]byte("echo " + line))
			}()
		}
	}()
	port := up.Addr().(*net.TCPAddr).Port

	dial := func(p *proxy) net.Conn {
		c, err := net.Dial("tcp", p.addr())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}

	p, err := newProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()

	// Allowed: hostname resolved by the proxy, data sent before the reply arrives.
	c := dial(p)
	if code := socksConnect(t, c, 1, "localhost", port, "hello\n"); code != repOK {
		t.Fatalf("localhost: code %d", code)
	}
	if got, _ := bufio.NewReader(c).ReadString('\n'); got != "echo hello\n" {
		t.Fatalf("got %q", got)
	}

	for _, tc := range []struct {
		name string
		cmd  byte
		host string
		port int
		want byte
	}{
		{"metadata", 1, "169.254.169.254", 80, repNotAllowed},
		{"bind", 2, "localhost", port, repBadCommand},
		{"port zero", 1, "localhost", 0, repNotAllowed},
		{"unresolvable", 1, "nonexistent.invalid", 80, repHostUnreach},
	} {
		if code := socksConnect(t, dial(p), tc.cmd, tc.host, tc.port, ""); code != tc.want {
			t.Errorf("%s: code %d, want %d", tc.name, code, tc.want)
		}
	}

	// Own ports are refused even though loopback is otherwise fine.
	own, err := newProxy([]uint16{uint16(port)})
	if err != nil {
		t.Fatal(err)
	}
	defer own.close()
	if code := socksConnect(t, dial(own), 1, "127.0.0.1", port, ""); code != repNotAllowed {
		t.Errorf("own port: code %d", code)
	}
	// ... also when named by hostname.
	if code := socksConnect(t, dial(own), 1, "localhost", port, ""); code != repNotAllowed {
		t.Errorf("own port via localhost: code %d", code)
	}

	// The listener only ever binds loopback.
	if host, _, _ := net.SplitHostPort(p.addr()); host != "127.0.0.1" {
		t.Errorf("proxy listens on %s", host)
	}
}
