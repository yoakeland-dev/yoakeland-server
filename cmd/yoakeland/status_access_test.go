package main

import (
	"bytes"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// The server-wide spend totals and the owner's subscription usage ride every status ping; a Limited
// person's pings carry neither, a Trusted person's and the local socket's carry both.
func TestStatusPingsHideSpendFromLimited(t *testing.T) {
	deps := newTestDeps(t)
	var lOut, tOut, localOut syncBuf
	lim := addPersonOn(t, deps, TierLimited, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &lOut))
	tr := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &tOut))
	deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &localOut), nopCloser{}, "unix", peerIdentity{Name: "pc"})
	for _, o := range []*syncBuf{&lOut, &tOut, &localOut} {
		o.Reset()
	}
	st := &protocol.ServerStatus{Spend: &protocol.ServerSpend{TodayUSD: 12.5}, RateLimits: map[string]protocol.ProviderRateLimits{"cli": {}}, Peers: 3}
	deps.peers.BroadcastEvent("", protocol.EventPing, "", protocol.PingData{Status: st})
	if s := lOut.String(); strings.Contains(s, "spend") || strings.Contains(s, "rateLimits") || !strings.Contains(s, `"peers":3`) {
		t.Errorf("the Limited person's ping: %s", s)
	}
	for name, o := range map[string]*syncBuf{"Trusted": &tOut, "local": &localOut} {
		if s := o.String(); !strings.Contains(s, `"todayUsd":12.5`) || !strings.Contains(s, "rateLimits") {
			t.Errorf("the %s ping lost its spend: %s", name, s)
		}
	}
	if st.Spend == nil || st.RateLimits == nil {
		t.Error("the shared status was modified")
	}
	_ = lim
	_ = tr
}
