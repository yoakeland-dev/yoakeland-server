package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"
)

// hostedAppURL is the static web client a phone opens from the QR code (HOSTED_APP_URL in
// src/lib/pairing.ts); it reads the address, token and fingerprint from the URL fragment and
// connects straight to this server over WebRTC.
const hostedAppURL = "https://app.yoake.land"

// pairingURL is the {t,h,p,fp} shape src/lib/pairing.ts decodes, same as the desktop's QR code.
func pairingURL(host string, port int, token, rtcFp string) string {
	b, _ := json.Marshal(struct {
		T  string `json:"t"`
		H  string `json:"h"`
		P  int    `json:"p"`
		FP string `json:"fp"`
	}{token, host, port, rtcFp})
	return hostedAppURL + "/#pair=" + base64.StdEncoding.EncodeToString(b)
}

// lanIPv4 is the first non-loopback IPv4 address of this machine, the one a phone on the same
// network can reach. "" if there is none.
func lanIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
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
	host := lanIPv4()
	if !ok || rtcFp == "" || host == "" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	token, err := newToken()
	if err != nil {
		return
	}
	q, err := qrcode.New(pairingURL(host, tcp.Port, token, rtcFp), qrcode.Low)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\nScan to open yoakeland on a phone and connect to %s:%d (good for %d minutes, one phone)\n%s\n", host, tcp.Port, int(PairingTTL.Minutes()), qrBlocks(q.Bitmap()))
}
