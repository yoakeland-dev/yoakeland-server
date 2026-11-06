package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// wire is one signed-in TCP connection a test can talk over.
type wire struct {
	t  *testing.T
	c  net.Conn
	r  *bufio.Reader
	me protocol.PeerInfo
}

func signInWire(t *testing.T, rc *remoteControl, token string) *wire {
	t.Helper()
	c := dialTLS(t, rc.ln.Addr().String())
	t.Cleanup(func() { c.Close() })
	me, refused := signIn(t, c, protocol.AuthData{Token: token, DeviceID: "dev-" + token, DeviceType: "web"})
	if refused != "" {
		t.Fatalf("sign in refused: %s", refused)
	}
	return &wire{t: t, c: c, r: bufio.NewReaderSize(c, 1<<20), me: me}
}

// ask sends one event and returns the first reply of type want, or the error message.
func (w *wire) ask(typ protocol.EventType, data any, want protocol.EventType) (json.RawMessage, string) {
	w.t.Helper()
	b, _ := json.Marshal(protocol.Envelope{Type: typ, Data: mustJSON(w.t, data)})
	if _, err := w.c.Write(append(b, '\n')); err != nil {
		w.t.Fatal(err)
	}
	_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := w.r.ReadBytes('\n')
		if err != nil {
			w.t.Fatalf("no %s reply to %s: %v", want, typ, err)
		}
		var env protocol.Envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case want:
			return env.Data, ""
		case protocol.EventError:
			var e protocol.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			return nil, e.Message
		}
	}
}

// dropped reports whether the server has closed this connection.
func (w *wire) dropped() bool {
	_ = w.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, err := w.r.ReadBytes('\n'); err != nil {
			ne, timeout := err.(net.Error)
			return !(timeout && ne.Timeout())
		}
	}
}

func (w *wire) chats() map[string]bool {
	w.t.Helper()
	raw, msg := w.ask(protocol.EventListSessions, map[string]any{}, protocol.EventSessionList)
	if msg != "" {
		w.t.Fatalf("list_sessions: %s", msg)
	}
	var d protocol.SessionListData
	_ = json.Unmarshal(raw, &d)
	out := map[string]bool{}
	for _, s := range d.Sessions {
		out[s.SessionID] = true
	}
	return out
}

// The owner pairs a second device of their own and a device for someone else, over the real
// listener: who each one is, what each sees, and what revoking does on the wire.
func TestTwoDevices_OwnersPhoneAndSomeoneElses(t *testing.T) {
	deps, rc := startPairingServer(t)
	quiesceOnCleanup(t, deps)
	deps.peers.UserName = deps.settings.UserName // main wires this; the roster names the person with it
	pc, err := deps.projects.Get(deps.defaultProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.projects.ensureChat(pc.SessStore, "owners-chat", ownerID); err != nil {
		t.Fatal(err)
	}

	register := func(body string) (token, cred string) {
		out := send(t, deps, "unix", nil, protocol.EventRegisterToken, body)
		if refused(out) {
			t.Fatalf("register_token %s: %s", body, out)
		}
		var env protocol.Envelope
		_ = json.Unmarshal([]byte(out[:indexNL(out)]), &env)
		var d struct{ Name, Token string }
		_ = json.Unmarshal(env.Data, &d)
		if d.Token == "" || d.Name == "" {
			t.Fatalf("no token in %s", out)
		}
		return d.Token, d.Name
	}
	myTok, myCred := register(`{"name":"my-phone","pairing":true,"user":"me"}`)
	samTok, samCred := register(`{"name":"sams-phone","pairing":true,"user":"new","newUserName":"Sam","tier":"limited"}`)

	mine := signInWire(t, rc, myTok)
	sams := signInWire(t, rc, samTok)

	if mine.me.UserID != ownerID {
		t.Fatalf("my phone is person %q, want the owner", mine.me.UserID)
	}
	if mine.me.Tier != TierLimited {
		t.Fatalf("my phone is %s, want the default device ceiling (limited)", mine.me.Tier)
	}
	if sams.me.UserID == ownerID || sams.me.UserName != "Sam" || sams.me.Tier != TierLimited {
		t.Fatalf("Sam's phone: %+v", sams.me)
	}

	// One person's devices share chats; a different person doesn't see them.
	if !mine.chats()["owners-chat"] {
		t.Fatal("the owner's own phone can't see the owner's chat")
	}
	if sams.chats()["owners-chat"] {
		t.Fatal("Sam can see a chat that isn't theirs")
	}
	if _, msg := sams.ask(protocol.EventGetSessionHistory, protocol.GetSessionHistoryData{SessionID: "owners-chat"}, protocol.EventSessionHistory); msg == "" {
		t.Fatal("Sam could read the owner's chat history")
	}

	// A limited device can't act as an admin, whoever its person is.
	if _, msg := mine.ask(protocol.EventRemoveUser, map[string]string{"userId": sams.me.UserID}, protocol.EventPeople); msg == "" {
		t.Fatal("a limited device of the owner could remove a person")
	}

	// Revoking Sam drops their open connection, and the owner's phone carries on.
	send(t, deps, "unix", nil, protocol.EventRemoveUser, `{"userId":"`+sams.me.UserID+`"}`)
	if !sams.dropped() {
		t.Fatal("a removed person's connection stayed open")
	}
	if c := dialTLS(t, rc.ln.Addr().String()); true {
		_, refusedMsg := signIn(t, c, protocol.AuthData{Token: samTok, DeviceID: "dev-" + samTok})
		c.Close()
		if refusedMsg == "" {
			t.Fatal("a removed person's credential still signs in")
		}
	}
	if !mine.chats()["owners-chat"] {
		t.Fatal("revoking someone else disturbed the owner's phone")
	}
	_, _ = myCred, samCred
}

func indexNL(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return len(s)
}
