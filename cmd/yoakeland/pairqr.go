package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
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

// pairingCode is the {t,ca,h,hs,p,fp} shape src/lib/pairing.ts decodes: what the QR code's URL
// carries, and, with the CA added, the one string the desktop app takes pasted (it pins the CA
// rather than the fingerprint). hosts[0] is the address to try first; any others ride along as
// "hs" for a client to race.
func pairingCode(hosts []string, port int, token, rtcFp, caPem string) string {
	type payload struct {
		T  string   `json:"t"`
		CA string   `json:"ca,omitempty"`
		H  string   `json:"h"`
		HS []string `json:"hs,omitempty"`
		P  int      `json:"p"`
		FP string   `json:"fp,omitempty"`
	}
	p := payload{T: token, CA: caPem, P: port, FP: rtcFp}
	if len(hosts) > 0 {
		p.H = hosts[0]
		p.HS = hosts[1:]
	}
	b, _ := json.Marshal(p)
	return base64.StdEncoding.EncodeToString(b)
}

// pairingURL is the hosted web client's link for a phone: the same code, without the CA (a
// browser can't use one; it pins the fingerprint over WebRTC).
func pairingURL(hosts []string, port int, token, rtcFp string) string {
	return hostedAppURL + "/#pair=" + pairingCode(hosts, port, token, rtcFp, "")
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

// sshServerIPv4 is the address the current ssh session reached this machine on (the third field
// of SSH_CONNECTION), if it is a usable IPv4 one: the one address known to work from where the
// operator is, including a public one lanIPv4s leaves out.
func sshServerIPv4(sshConnection string) string {
	f := strings.Fields(sshConnection)
	if len(f) != 4 {
		return ""
	}
	if ip := net.ParseIP(f[2]); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
		return ip.String()
	}
	return ""
}

// pairHosts is lanIPv4s with the ssh session's own address first, so a server on a cloud box with
// no private address still has one to offer.
func pairHosts() []string {
	hosts := append(lanIPv4s(), globalIPv6s()...)
	if ip := sshServerIPv4(os.Getenv("SSH_CONNECTION")); ip != "" {
		hosts = append([]string{ip}, slices.DeleteFunc(hosts, func(h string) bool { return h == ip })...)
		if len(hosts) > maxPairHosts {
			hosts = hosts[:maxPairHosts]
		}
	}
	return hosts
}

// globalIPv6s is at most one public IPv6 address of this machine, listed last: behind carrier-grade
// NAT it is the only address a client elsewhere can reach without a port forward.
// ponytail: first one found, which may be a temporary privacy address; pick a stable one if it rotates under users.
func globalIPv6s() []string {
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() == nil && n.IP.IsGlobalUnicast() && !n.IP.IsPrivate() {
				return []string{n.IP.String()}
			}
		}
	}
	return nil
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

// printPairQR shows a pairing code on stderr beside the log line, only when someone is watching a
// terminal: a QR code for a phone (when the server can be reached over WebRTC) and the same thing
// as a string to paste into the desktop app, which is what you have over ssh, and a short PIN to
// type there with the address (pairpake.go). Its token comes from newToken, called only once it is
// certain to print: a one-device token that expires, not the shared one. The PIN is minted only
// then too, and if it runs out of guesses a new one is printed in its place.
func printPairQR(addr net.Addr, newToken func() (string, error), rtcFp, caPem string, pins *pinRegistry) {
	tcp, ok := addr.(*net.TCPAddr)
	hosts := pairHosts()
	if !ok || len(hosts) == 0 || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	token, err := newToken()
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\nPair a device with %s (valid for %d minutes, one device)\n", net.JoinHostPort(hosts[0], strconv.Itoa(tcp.Port)), int(PairingTTL.Minutes()))
	if rtcFp != "" {
		if q, err := qrcode.New(pairingURL(hosts, tcp.Port, token, rtcFp), qrcode.Low); err == nil {
			fmt.Fprintf(os.Stderr, "Scan with a phone:\n%s\n", qrBlocks(q.Bitmap()))
		}
	}
	fmt.Fprintf(os.Stderr, "Or paste into the app's Pairing code field:\n%s\n", pairingCode(hosts, tcp.Port, token, rtcFp, caPem))
	if pins != nil {
		fmt.Fprint(os.Stderr, pinAnnouncement(hosts[0], tcp.Port, pins.Mint()))
		pins.OnExhausted(func(pin string) {
			fmt.Fprintf(os.Stderr, "\nThe pairing PIN was entered wrong too many times and is no longer valid.\n%s", pinAnnouncement(hosts[0], tcp.Port, pin))
		})
	}
	fmt.Fprintln(os.Stderr)
}

// pinAnnouncement is what to type into the app instead of scanning or pasting.
func pinAnnouncement(host string, port int, pin string) string {
	return fmt.Sprintf("Or type these into the app's Add remote form (the PIN works once, %d tries):\n  Address  %s\n  PIN      %s\n", pinAttempts, net.JoinHostPort(host, strconv.Itoa(port)), formatPIN(pin))
}
