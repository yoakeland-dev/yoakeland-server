package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"
)

// hostedAppURL is the static web client a phone opens from the QR code (HOSTED_APP_URL in
// src/lib/pairing.ts); it reads the address, token and fingerprint from the URL fragment and
// connects straight to this server over WebRTC.
const hostedAppURL = "https://app.yoake.land"

// maxPairHosts bounds the addresses in a QR code: each one makes it denser.
const maxPairHosts = 3

// pairingURL is the {t,h,p,fp} shape src/lib/pairing.ts decodes, same as the desktop's QR code.
// hosts[0] is the address to try first; any others ride along as "hs" for a phone to race.
func pairingURL(hosts []string, port int, token, rtcFp string) string {
	type payload struct {
		T  string   `json:"t"`
		H  string   `json:"h"`
		HS []string `json:"hs,omitempty"`
		P  int      `json:"p"`
		FP string   `json:"fp"`
	}
	p := payload{T: token, P: port, FP: rtcFp}
	if len(hosts) > 0 {
		p.H = hosts[0]
		p.HS = hosts[1:]
	}
	b, _ := json.Marshal(p)
	return hostedAppURL + "/#pair=" + base64.StdEncoding.EncodeToString(b)
}

// virtualIface is an interface that only the machine itself, or its containers, can reach: an
// address there is never the one a phone on the network needs.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vboxnet", "vmnet", "cni", "flannel", "lxc", "lxd", "podman"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// lanIPv4s lists the addresses a phone on the same network could reach this machine at: private
// IPv4 ones, the one the default route leaves by first, then the rest, then carrier-grade NAT
// ones (Tailscale and the like) that only help a phone on that network. Containers, loopback and
// link-local addresses are left out.
func lanIPv4s() []string {
	defaultSrc := ""
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil { // no packet is sent; this only picks a route
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
			defaultSrc = a.IP.String()
		}
		c.Close()
	}
	ifaces, _ := net.Interfaces()
	var private, shared []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
				continue
			}
			switch ip := n.IP.String(); {
			case n.IP.IsPrivate():
				private = append(private, ip)
			case isCGNAT(n.IP):
				shared = append(shared, ip)
			}
		}
	}
	sort.SliceStable(private, func(i, j int) bool { return private[i] == defaultSrc && private[j] != defaultSrc })
	out := append(private, shared...)
	if len(out) > maxPairHosts {
		out = out[:maxPairHosts]
	}
	return out
}

func isCGNAT(ip net.IP) bool {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

// qrBlocks draws a code in half-block characters, two rows per line, always black on white so a
// phone camera reads it on any terminal theme.
func qrBlocks(bitmap [][]bool) string {
	var b strings.Builder
	at := func(x, y int) bool { return y < len(bitmap) && bitmap[y][x] }
	for y := 0; y < len(bitmap); y += 2 {
		b.WriteString("\x1b[30;47m")
		for x := range bitmap[y] {
			switch top, bot := at(x, y), at(x, y+1); {
			case top && bot:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bot:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}

// printPairQR shows a pairing QR code on stderr beside the log line, only when someone is
// watching a terminal and the server can be reached over WebRTC. Its token comes from newToken,
// called only once it is certain to print: a one-device token that expires, not the shared one.
func printPairQR(addr net.Addr, newToken func() (string, error), rtcFp string) {
	tcp, ok := addr.(*net.TCPAddr)
	hosts := lanIPv4s()
	if !ok || rtcFp == "" || len(hosts) == 0 || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	token, err := newToken()
	if err != nil {
		return
	}
	q, err := qrcode.New(pairingURL(hosts, tcp.Port, token, rtcFp), qrcode.Low)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\nScan to open yoakeland on a phone and connect to %s:%d (good for %d minutes, one phone)\n%s\n", hosts[0], tcp.Port, int(PairingTTL.Minutes()), qrBlocks(q.Bitmap()))
}
