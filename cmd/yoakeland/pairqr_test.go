package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

func TestPairingURLDecodes(t *testing.T) {
	u, err := url.Parse(pairingURL([]string{"192.168.1.5", "10.0.0.2"}, 7890, "tok", "AB:CD"))
	if err != nil || u.Host != "app.yoake.land" {
		t.Fatalf("bad url %v %v", u, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u.Fragment, "pair="))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		T, H, FP string
		HS       []string
		P        int
	}
	if json.Unmarshal(raw, &got) != nil || got.T != "tok" || got.H != "192.168.1.5" || len(got.HS) != 1 || got.HS[0] != "10.0.0.2" || got.P != 7890 || got.FP != "AB:CD" {
		t.Fatalf("got %+v", got)
	}
}

func TestQRBlocksPairsRows(t *testing.T) {
	q, _ := qrcode.New("x", qrcode.Medium)
	bm := q.Bitmap()
	if lines := strings.Count(qrBlocks(bm), "\n"); lines != (len(bm)+1)/2 {
		t.Fatalf("%d lines for %d rows", lines, len(bm))
	}
}

func TestVirtualIfaceAndCGNAT(t *testing.T) {
	for _, n := range []string{"docker0", "br-1a2b", "veth9f", "virbr0"} {
		if !virtualIface(n) {
			t.Errorf("%s should be skipped", n)
		}
	}
	for _, n := range []string{"wlan0", "enp3s0", "eth0"} {
		if virtualIface(n) {
			t.Errorf("%s should be kept", n)
		}
	}
	if !isCGNAT(net.ParseIP("100.101.2.3")) || isCGNAT(net.ParseIP("192.168.1.2")) {
		t.Error("isCGNAT misclassifies")
	}
	got := lanIPv4s()
	t.Logf("lanIPv4s = %v", got)
	if len(got) > maxPairHosts {
		t.Errorf("lanIPv4s returned %d addresses, cap is %d", len(got), maxPairHosts)
	}
}

func TestPairingCodeCarriesCAForTheDesktop(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(pairingCode([]string{"203.0.113.7"}, 7890, "tok", "", "PEM"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil || got["t"] != "tok" || got["ca"] != "PEM" || got["h"] != "203.0.113.7" || got["p"] != float64(7890) {
		t.Fatalf("got %v", got)
	}
	if _, has := got["fp"]; has {
		t.Errorf("an empty fingerprint should be left out: %v", got)
	}
	if strings.Contains(pairingURL([]string{"10.0.0.2"}, 7890, "tok", "AB:CD"), "PEM") {
		t.Error("the phone's link must not carry the CA")
	}
}

func TestSSHServerIPv4(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.4 51234 203.0.113.7 22": "203.0.113.7",
		"198.51.100.4 51234 127.0.0.1 22":   "", // tunnelled: not an address the desktop can dial
		"198.51.100.4 51234 2001:db8::1 22": "",
		"":                                  "",
	} {
		if got := sshServerIPv4(in); got != want {
			t.Errorf("sshServerIPv4(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("SSH_CONNECTION", "198.51.100.4 51234 203.0.113.7 22")
	if h := pairHosts(); len(h) == 0 || h[0] != "203.0.113.7" || len(h) > maxPairHosts {
		t.Errorf("the ssh address should come first: %v", h)
	}
}
