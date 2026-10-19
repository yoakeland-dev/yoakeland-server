package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"yoakeland/internal/protocol"
)

func TestRemoteScope(t *testing.T) {
	deps := newTestDeps(t)
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", DeviceType: "phone", Restricted: true})
	person := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "laptop", DeviceType: "desktop"})

	for _, tc := range []struct {
		name    string
		network string
		peer    string
		event   protocol.EventType
		denied  bool
	}{
		{"phone may chat", "tcp", phone.ID, protocol.EventUserPrompt, false},
		{"phone cannot add an MCP server", "tcp", phone.ID, protocol.EventSaveMCPServer, true},
		{"phone cannot register a provider", "tcp", phone.ID, protocol.EventRegisterProvider, true},
		{"phone cannot change runtime settings", "tcp", phone.ID, protocol.EventSetRuntimeSettings, true},
		{"phone cannot add a global rule", "tcp", phone.ID, protocol.EventAddPermissionRule, true},
		{"phone cannot open a terminal", "tcp", phone.ID, protocol.EventTerminalOpen, true},
		{"phone cannot type into one", "tcp", phone.ID, protocol.EventTerminalInput, true},
		{"phone cannot drop a peer", "tcp", phone.ID, protocol.EventDisconnectPeer, true},
		{"an unlisted event is denied", "tcp", phone.ID, protocol.EventType("something_new"), true},
		{"an unregistered tcp connection is denied", "tcp", "", protocol.EventSaveMCPServer, true},
		{"a full credential is not restricted", "tcp", person.ID, protocol.EventSaveMCPServer, false},
		{"the local socket is not restricted", "unix", "", protocol.EventSaveMCPServer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteDenied(deps, tc.network, tc.peer, tc.event); got != tc.denied {
				t.Fatalf("remoteDenied = %v, want %v", got, tc.denied)
			}
		})
	}
}

// The gate must reach the handler path, not just the helper.
func TestRemoteScope_DispatchRefuses(t *testing.T) {
	deps := newTestDeps(t)
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", Restricted: true})
	out := &bytes.Buffer{}
	in := bytes.NewBufferString(`{"type":"save_mcp_server","data":{}}` + "\n")
	serveConnection(context.Background(), protocol.NewIPCStream(in, out), deps, "tcp", phone.ID, nil)
	var env protocol.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil || env.Type != protocol.EventError || !strings.Contains(string(env.Data), "isn't available from a paired device") {
		t.Fatalf("expected a refusal, got %q (%v)", out.String(), err)
	}
}

func TestHandshake_PairingCredentialIsRestricted(t *testing.T) {
	for _, tc := range []struct {
		pairing bool
	}{{true}, {false}} {
		lookup := func(string) (string, TokenRecord, bool) { return "tok", TokenRecord{Pairing: tc.pairing}, true }
		in := bytes.NewBufferString(`{"type":"auth","data":{"token":"x"}}` + "\n")
		who, _, ok := handshake(protocol.NewIPCStream(in, &bytes.Buffer{}), "tcp", lookup)
		if !ok || who.Restricted != tc.pairing {
			t.Fatalf("pairing=%v: ok=%v restricted=%v", tc.pairing, ok, who.Restricted)
		}
	}
}

func TestSetPeerScope(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.settings.AddPairingToken("phone", "t1", ""); err != nil {
		t.Fatal(err)
	}
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", TokenName: "phone", Restricted: true})
	laptop := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "laptop"})
	send := func(from *Peer, network string, full bool) {
		in := bytes.NewBufferString(`{"type":"set_peer_scope","data":{"peerId":"` + phone.ID + `","full":` + map[bool]string{true: "true", false: "false"}[full] + "}}\n")
		serveConnection(context.Background(), protocol.NewIPCStream(in, &bytes.Buffer{}), deps, network, from.ID, nil)
	}

	send(phone, "tcp", true) // a restricted device can't promote itself
	if !deps.peers.IsRestricted(phone.ID) {
		t.Fatal("restricted device promoted itself")
	}
	send(laptop, "tcp", true)
	if deps.peers.IsRestricted(phone.ID) || remoteDenied(deps, "tcp", phone.ID, protocol.EventSaveMCPServer) {
		t.Fatal("promotion did not take effect live")
	}
	if rec := deps.settings.Tokens["phone"]; rec.Restricted() {
		t.Fatal("promotion was not persisted")
	}
	send(laptop, "tcp", false)
	if !deps.peers.IsRestricted(phone.ID) {
		t.Fatal("demotion did not take effect")
	}
}

func TestResolveAuthor_AvatarFallback(t *testing.T) {
	deps := newTestDeps(t)
	p := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", TokenName: "phone"})
	if a := resolveAuthor(deps, p.ID, "x", "🦊"); a.Name != "phone" || a.Avatar != "🦊" {
		t.Fatalf("no registered avatar: got %+v", a)
	}
	deps.peers.SetAvatar(p.ID, "data:image/png;base64,AA")
	if a := resolveAuthor(deps, p.ID, "x", "🦊"); a.Avatar != "data:image/png;base64,AA" {
		t.Fatalf("registered avatar should win: got %+v", a)
	}
}

// A remote connection browses only inside projects open here, and can't remove one it doesn't know.
func TestRemoteScope_ProjectBoundaries(t *testing.T) {
	deps := newTestDeps(t)
	person := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", DeviceType: "mobile", Restricted: true})
	outside := t.TempDir()
	for _, tc := range []struct{ envelope, want string }{
		{`{"type":"list_directory","data":{"path":"` + outside + `"}}`, "isn't available from a paired device"},
		{`{"type":"list_tree","data":{"projectPath":"` + outside + `"}}`, "isn't open on this server"},
	} {
		out := &bytes.Buffer{}
		serveConnection(context.Background(), protocol.NewIPCStream(bytes.NewBufferString(tc.envelope+"\n"), out), deps, "tcp", person.ID, nil)
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s: expected %q, got %q", tc.envelope, tc.want, out.String())
		}
	}
	// The project the server has open is still browsable.
	if !deps.projects.Within(deps.defaultProjectRoot) || deps.projects.Within(outside) {
		t.Fatal("Within does not match the open project's tree")
	}
}

// A device's self-reported identity is capped before it reaches the roster, and the name a
// device falls back to is cleaned like any other.
func TestPeerIdentity_Capped(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	ipc := protocol.NewIPCStream(bytes.NewBufferString(`{"type":"auth","data":{"deviceType":"`+long+`","deviceId":"`+long+`"}}`+"\n"), &bytes.Buffer{})
	who, _, ok := handshake(ipc, "unix", nil)
	if !ok || len(who.DeviceID) > maxDeviceIDLen || utf8.RuneCountInString(who.DeviceType) > maxNameLen {
		t.Fatalf("identity not capped: ok=%v idLen=%d typeLen=%d", ok, len(who.DeviceID), len(who.DeviceType))
	}
	reg := NewPeerRegistry()
	p := reg.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{DeviceType: long})
	if utf8.RuneCountInString(p.Name) > maxNameLen {
		t.Fatalf("fallback name is %d runes, want at most %d", utf8.RuneCountInString(p.Name), maxNameLen)
	}
	// A bidi override can't make a name read as another one.
	if got := cleanName("pay\u202Ealice"); got != "payalice" {
		t.Fatalf("cleanName kept a format character: %q", got)
	}
}

// One credential may hold only maxConnsPerToken live connections.
func TestPeerRegistry_AtCredentialLimit(t *testing.T) {
	reg := NewPeerRegistry()
	for i := 0; i < maxConnsPerToken; i++ {
		reg.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{TokenName: "phone"})
	}
	if !reg.AtCredentialLimit("phone") || reg.AtCredentialLimit("laptop") {
		t.Fatal("limit is not per credential")
	}
}

func TestRegisterToken_PairingTier(t *testing.T) {
	deps := newTestDeps(t)
	owner := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "unix", peerIdentity{Name: "owner"})
	for name, tc := range map[string]struct{ body, want string }{
		"phone":  {`"pairing":true`, "limited"},
		"laptop": {`"pairing":true,"full":true`, "full"},
	} {
		in := bytes.NewBufferString(`{"type":"register_token","data":{"name":"` + name + `",` + tc.body + "}}\n")
		serveConnection(context.Background(), protocol.NewIPCStream(in, &bytes.Buffer{}), deps, "unix", owner.ID, nil)
		if restricted := deps.settings.TokenRestricted(name); restricted != (tc.want == "limited") {
			t.Fatalf("%s: restricted=%v, want %s", name, restricted, tc.want)
		}
	}
}

func TestServerIdentity_CarriesTier(t *testing.T) {
	deps := newTestDeps(t)
	var out bytes.Buffer
	p := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &out), nopCloser{}, "tcp", peerIdentity{Name: "phone", Restricted: true})
	sendServerIdentity(deps, p)
	if !strings.Contains(out.String(), `"restricted":true`) {
		t.Fatalf("limited device not told its tier: %q", out.String())
	}
}

// A full-access remote device is the owner and may browse outside the open projects.
func TestRemoteScope_FullAccessBrowsesAnywhere(t *testing.T) {
	deps := newTestDeps(t)
	laptop := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "laptop", DeviceType: "desktop"})
	out := &bytes.Buffer{}
	env := `{"type":"list_directory","data":{"path":"` + t.TempDir() + `"}}`
	serveConnection(context.Background(), protocol.NewIPCStream(bytes.NewBufferString(env+"\n"), out), deps, "tcp", laptop.ID, nil)
	if !strings.Contains(out.String(), `"directory_list"`) || strings.Contains(out.String(), "can only browse") {
		t.Fatalf("full-access device was confined: %q", out.String())
	}
}

// A server used only remotely has no local client: a full-access device manages people, and can
// be asked to approve a pairing. A limited one can do neither.
func TestFullRemoteDeviceManagesPeople(t *testing.T) {
	deps := newTestDeps(t)
	owner := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "owner", DeviceType: "desktop"})
	phone := deps.peers.Add(protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}), nopCloser{}, "tcp", peerIdentity{Name: "phone", DeviceType: "phone", Restricted: true})
	run := func(from *Peer, env string) string {
		out := &bytes.Buffer{}
		serveConnection(context.Background(), protocol.NewIPCStream(bytes.NewBufferString(env+"\n"), out), deps, "tcp", from.ID, nil)
		return out.String()
	}
	if got := run(owner, `{"type":"register_token","data":{"name":"mac","pairing":true,"full":true}}`); !strings.Contains(got, `"token_registered"`) {
		t.Fatalf("full device couldn't register a token: %q", got)
	}
	if got := run(owner, `{"type":"list_tokens","data":{}}`); !strings.Contains(got, `"name":"mac"`) {
		t.Fatalf("list_tokens: %q", got)
	}
	if got := run(phone, `{"type":"register_token","data":{"name":"x"}}`); !strings.Contains(got, "isn't available from a paired device") {
		t.Fatalf("limited device registered a token: %q", got)
	}
	if got := run(owner, `{"type":"remove_token","data":{"name":"mac"}}`); !strings.Contains(got, `"token_removed"`) {
		t.Fatalf("remove_token: %q", got)
	}
	if !deps.peers.HasLocal() {
		t.Fatal("a full remote device should count as someone who can approve a pairing")
	}
}
