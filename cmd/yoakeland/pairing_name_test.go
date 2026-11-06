package main

import (
	"encoding/json"
	"strings"
	"testing"

	"yoakeland/internal/protocol"
)

// A code made for someone new names nobody: the person is unlisted until their device connects,
// takes the name it sends, and doesn't outlive a code that was never used.
func TestPairing_NewPersonTakesTheNameTheirDeviceSends(t *testing.T) {
	deps, rc := startPairingServer(t)
	quiesceOnCleanup(t, deps)
	deps.peers.UserName = deps.settings.UserName // main wires this

	listed := func(name string) bool {
		b, _ := json.Marshal(deps.peopleFor(deps.accessFor("unix", "")))
		return strings.Contains(string(b), `"name":"`+name+`"`)
	}
	tokenFor := func(cred string) string {
		t.Helper()
		out := send(t, deps, "unix", nil, protocol.EventRegisterToken, `{"name":"`+cred+`","pairing":true,"user":"new","tier":"limited"}`)
		if refused(out) {
			t.Fatalf("register_token: %s", out)
		}
		rec, ok := deps.settings.TokenRecordOf(cred)
		if !ok {
			t.Fatalf("no record for %s", cred)
		}
		return rec.Token
	}

	tok := tokenFor("New device")
	if listed("New device") {
		t.Fatal("a person waiting for their device is listed")
	}
	c := dialTLS(t, rc.ln.Addr().String())
	t.Cleanup(func() { c.Close() })
	me, why := signIn(t, c, protocol.AuthData{Token: tok, Name: "Sam Rivera", DeviceID: "sams-phone", DeviceType: "web"})
	if why != "" {
		t.Fatalf("sign in refused: %s", why)
	}
	if me.UserName != "Sam Rivera" {
		t.Fatalf("the person is called %q, want the name the device sent", me.UserName)
	}
	if !listed("Sam Rivera") {
		t.Fatal("the person isn't listed once their device joined")
	}

	// A later connection under another name doesn't rename them.
	if deps.settings.TakeDeviceName("New device", "Someone Else", "") {
		t.Fatal("the name was taken twice")
	}

	before := len(deps.settings.Users)
	tokenFor("Second code")
	if len(deps.settings.Users) != before+1 {
		t.Fatal("no person was made for the code")
	}
	deps.settings.RemoveUnclaimedPairing("Second code")
	if len(deps.settings.Users) != before {
		t.Fatal("a declined code left its person behind")
	}
	tokenFor("Third code")
	if _, err := deps.settings.RemoveToken("Third code"); err != nil {
		t.Fatal(err)
	}
	if len(deps.settings.Users) != before {
		t.Fatal("a cancelled code left its person behind")
	}
}
