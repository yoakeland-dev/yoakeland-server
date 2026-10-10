package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

func TestPairingURLDecodes(t *testing.T) {
	u, err := url.Parse(pairingURL("192.168.1.5", 7890, "tok", "AB:CD"))
	if err != nil || u.Host != "app.yoake.land" {
		t.Fatalf("bad url %v %v", u, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u.Fragment, "pair="))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		T, H, FP string
		P        int
	}
	if json.Unmarshal(raw, &got) != nil || got.T != "tok" || got.H != "192.168.1.5" || got.P != 7890 || got.FP != "AB:CD" {
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
