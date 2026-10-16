package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// auditedDeps is newTestDeps with a real audit log wired in the way main does.
func auditedDeps(t *testing.T) (connDeps, *audit.Log, string) {
	t.Helper()
	deps := newTestDeps(t)
	dir := filepath.Join(t.TempDir(), "audit")
	log, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	deps.audit = log
	deps.projects.SetAudit(log)
	return deps, log, dir
}

// asPeer runs lines through serveConnection as a registered device.
func asPeer(t *testing.T, deps connDeps, p *Peer, lines ...string) {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	serveConnection(context.Background(), protocol.NewIPCStream(in, &lockedBuf{}), deps, "unix", p.ID, nil)
}

func newDevice(t *testing.T, deps connDeps, name string) *Peer {
	t.Helper()
	p, _ := fakeIPCPeer(t, deps.peers, name, "desktop")
	return p
}

func eventsOf(t *testing.T, log *audit.Log, kind string) []audit.Event {
	t.Helper()
	got, _, err := log.Read(audit.Query{Kinds: []string{kind}})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// toolServer is an Anthropic-shaped endpoint whose first reply asks to run a
// bash command and whose later ones just say "done".
func toolServer(command string) *httptest.Server {
	var calls int32
	input, _ := json.Marshal(map[string]string{"command": command})
	partial, _ := json.Marshal(string(input))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			for _, ev := range []string{
				`event: content_block_start` + "\n" + `data: {"index":0,"content_block":{"type":"tool_use","id":"tu1","name":"bash","input":{}}}`,
				`event: content_block_delta` + "\n" + `data: {"index":0,"delta":{"type":"input_json_delta","partial_json":` + string(partial) + `}}`,
				`event: content_block_stop` + "\n" + `data: {"index":0}`,
				`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				`event: message_stop` + "\n" + `data: {}`,
			} {
				_, _ = w.Write([]byte(ev + "\n\n"))
			}
			return
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: message_stop\ndata: {}\n\n"))
	}))
}

// startTurn registers a provider against srv and sends a prompt on chat1, in
// the given mode, from device p. It returns once the turn has been dispatched.
func startTurn(t *testing.T, deps connDeps, p *Peer, srv *httptest.Server, mode string) {
	t.Helper()
	off := false
	asPeer(t, deps, p,
		envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{Name: "p", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic", APIKey: "k", BaseURL: srv.URL}),
		envelopeJSONWithSession(t, protocol.EventSetProvider, "chat1", protocol.SetProviderData{ProviderName: "p", Model: "claude-sonnet-5"}),
		envelopeJSONWithSession(t, protocol.EventSetMode, "chat1", protocol.SetModeData{SessionID: "chat1", Mode: mode}),
		envelopeJSON(t, protocol.EventUserPrompt, protocol.UserPromptData{Prompt: "run it", SessionID: "chat1", AutoGenerateTitle: &off}),
	)
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The answer to a permission ask is recorded with the device that gave it, the
// tool and command it was about, and the id of the rule it stored; the run
// that followed is recorded too.
func TestAudit_PermissionDecisionNamesTheAnsweringDevice(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	phone := newDevice(t, deps, "phone")
	laptop := newDevice(t, deps, "laptop")
	srv := toolServer("echo hi")
	defer srv.Close()

	startTurn(t, deps, phone, srv, "accept_edits") // bash still asks
	waitUntil(t, "the ask", func() bool { return len(deps.projects.PendingAsks()) == 1 })

	answer, _ := json.Marshal(protocol.Envelope{Type: protocol.EventPermissionResponse, SessionID: "chat1", ToolCallID: "tu1",
		Data: json.RawMessage(`{"allowed":true,"remember":true}`)})
	asPeer(t, deps, laptop, string(answer))
	deps.turnWG.Wait()

	dec := eventsOf(t, log, audit.PermDecision)
	if len(dec) != 1 {
		t.Fatalf("want one decision, got %+v", dec)
	}
	d := dec[0]
	if d.Actor != "laptop" || d.Outcome != "allow" || d.Tool != "bash" || d.Target != "echo hi" || d.Session != "chat1" || d.Project == "" {
		t.Fatalf("decision: %+v", d)
	}
	ruleID, _ := d.Detail["ruleId"].(string)
	if ruleID == "" || d.Detail["remember"] != true {
		t.Fatalf("a remembered answer should name its rule: %+v", d.Detail)
	}
	added := eventsOf(t, log, audit.RuleAdd)
	if len(added) != 1 || added[0].Actor != "laptop" || added[0].Detail["ruleId"] != ruleID || added[0].Outcome != "allow" {
		t.Fatalf("rule_add: %+v", added)
	}
	ran := eventsOf(t, log, audit.ToolExec)
	if len(ran) != 1 || ran[0].Tool != "bash" || ran[0].Target != "echo hi" || ran[0].Outcome != "run" || ran[0].Detail["via"] != "user" {
		t.Fatalf("tool_exec: %+v", ran)
	}
	// The mode the session was put in, by the phone.
	if modes := eventsOf(t, log, audit.ModeChange); len(modes) != 1 || modes[0].Actor != "phone" || modes[0].Detail["from"] != "read_write" || modes[0].Detail["to"] != "accept_edits" {
		t.Fatalf("mode_change: %+v", modes)
	}
}

// Autonomous mode answers every ask itself. That is the decision most worth
// being able to look back on, so it is recorded, and the run is too.
func TestAudit_AutonomousBypassIsRecorded(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	phone := newDevice(t, deps, "phone")
	srv := toolServer("echo hi")
	defer srv.Close()

	startTurn(t, deps, phone, srv, "autonomous")
	deps.turnWG.Wait()

	auto := eventsOf(t, log, audit.PermAuto)
	if len(auto) != 1 || auto[0].Outcome != "allow" || auto[0].Tool != "bash" || auto[0].Detail["via"] != "autonomous" || auto[0].Detail["mode"] != "autonomous" {
		t.Fatalf("perm_auto: %+v", auto)
	}
	if ran := eventsOf(t, log, audit.ToolExec); len(ran) != 1 || ran[0].Detail["via"] != "autonomous" {
		t.Fatalf("tool_exec: %+v", ran)
	}
	if asked := eventsOf(t, log, audit.PermDecision); len(asked) != 0 {
		t.Fatalf("nobody was asked: %+v", asked)
	}
}

// A rule added from the rules UI is recorded with who added it and its id; the
// same rule later refusing a command is recorded as an automatic denial
// naming that id, and nothing runs.
func TestAudit_RuleChangesAndRuleAnsweredAsks(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	laptop := newDevice(t, deps, "laptop")
	srv := toolServer("echo hi")
	defer srv.Close()

	add := envelopeJSON(t, protocol.EventAddPermissionRule, protocol.AddPermissionRuleData{
		ProjectPath: deps.defaultProjectRoot, Scope: "project",
		Rule: protocol.PermissionRuleData{Tool: "bash", Text: "echo *", Decision: "deny"},
	})
	asPeer(t, deps, laptop, add)
	added := eventsOf(t, log, audit.RuleAdd)
	if len(added) != 1 || added[0].Actor != "laptop" || added[0].Outcome != "deny" || added[0].Tool != "bash" {
		t.Fatalf("rule_add: %+v", added)
	}
	ruleID, _ := added[0].Detail["ruleId"].(string)
	if ruleID == "" {
		t.Fatalf("no rule id: %+v", added[0])
	}

	startTurn(t, deps, laptop, srv, "read_write")
	deps.turnWG.Wait()
	auto := eventsOf(t, log, audit.PermAuto)
	if len(auto) != 1 || auto[0].Outcome != "deny" || auto[0].Detail["ruleId"] != ruleID || auto[0].Target != "echo hi" {
		t.Fatalf("perm_auto: %+v", auto)
	}
	if ran := eventsOf(t, log, audit.ToolExec); len(ran) != 0 {
		t.Fatalf("a denied call must not be recorded as run: %+v", ran)
	}

	asPeer(t, deps, laptop, envelopeJSON(t, protocol.EventDeletePermissionRule, protocol.DeletePermissionRuleData{ProjectPath: deps.defaultProjectRoot, ID: ruleID}))
	removed := eventsOf(t, log, audit.RuleRemove)
	if len(removed) != 1 || removed[0].Actor != "laptop" || removed[0].Detail["ruleId"] != ruleID {
		t.Fatalf("rule_remove: %+v", removed)
	}
}

// Credentials are acted on, and the fact is recorded, but never their values.
func TestAudit_SecretsNeverReachTheLog(t *testing.T) {
	deps, log, dir := auditedDeps(t)
	laptop := newDevice(t, deps, "laptop")
	const password = "correct-horse-battery-staple"
	const apiKey = "sk-ant-SECRETKEY0123456789abcdef"

	in := strings.NewReader(strings.Join([]string{
		envelopeJSON(t, protocol.EventSetRemotePassword, protocol.SetRemotePasswordData{Password: password}),
		envelopeJSON(t, protocol.EventRegisterProvider, protocol.RegisterProviderData{Name: "work", Kind: string(provider.ProviderKindAPIKey), HTTPProvider: "anthropic", APIKey: apiKey, BaseURL: "https://user:urlsecret@api.example.com"}),
		envelopeJSON(t, protocol.EventRegisterToken, protocol.RegisterTokenData{Name: "tablet"}),
	}, "\n") + "\n")
	out := &bytes.Buffer{}
	serveConnection(context.Background(), protocol.NewIPCStream(in, out), deps, "unix", laptop.ID, nil)

	var token string
	for _, line := range strings.Split(out.String(), "\n") {
		var env protocol.Envelope
		if json.Unmarshal([]byte(line), &env) == nil && env.Type == protocol.EventTokenRegistered {
			var d protocol.TokenRegisteredData
			_ = json.Unmarshal(env.Data, &d)
			token = d.Token
		}
	}
	if token == "" {
		t.Fatalf("no token minted:\n%s", out.String())
	}

	for kind, want := range map[string]string{audit.RemotePassword: "", audit.ProviderAdd: "work", audit.TokenAdd: "tablet"} {
		got := eventsOf(t, log, kind)
		if len(got) != 1 || got[0].Actor != "laptop" || got[0].Target != want {
			t.Fatalf("%s: %+v", kind, got)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{password, apiKey, "SECRETKEY", "urlsecret", token} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q reached the audit log:\n%s", secret, raw)
		}
	}
}

func TestAudit_LimitPauseAnswerAndSettingsChangeByKeyOnly(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	laptop := newDevice(t, deps, "laptop")
	asPeer(t, deps, laptop,
		`{"type":"ask_user_answer","sessionId":"chat1","toolCallId":"limit-t1-1","data":{"answer":"Continue"}}`,
		`{"type":"set_runtime_settings","data":{"maxRoundsPerTurn":7331}}`,
	)
	lim := eventsOf(t, log, audit.LimitPause)
	if len(lim) != 1 || lim[0].Actor != "laptop" || lim[0].Outcome != "continue" || lim[0].Session != "chat1" {
		t.Fatalf("limit_pause: %+v", lim)
	}
	st := eventsOf(t, log, audit.SettingsChange)
	if len(st) != 1 || st[0].Actor != "laptop" {
		t.Fatalf("settings_change: %+v", st)
	}
	if keys, _ := st[0].Detail["keys"].([]any); len(keys) != 1 || keys[0] != "maxRoundsPerTurn" {
		t.Fatalf("keys: %+v", st[0].Detail)
	}
	if raw, _ := json.Marshal(st[0]); strings.Contains(string(raw), "7331") {
		t.Fatalf("a setting's value was logged: %s", raw)
	}
}

func TestAudit_ListAuditPagesAndFilters(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	for i := 0; i < 5; i++ {
		log.Append(audit.Event{Kind: audit.ToolExec, Session: "a", Project: "/p"})
	}
	log.Append(audit.Event{Kind: audit.ModeChange, Session: "b", Project: "/q"})

	ask := func(d protocol.ListAuditData) protocol.AuditListData {
		t.Helper()
		envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListAudit, d))
		if len(envs) != 1 || envs[0].Type != protocol.EventAuditList {
			t.Fatalf("want one audit_list, got %+v", envs)
		}
		var out protocol.AuditListData
		if err := json.Unmarshal(envs[0].Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	p1 := ask(protocol.ListAuditData{RequestID: "r1", Limit: 4})
	if p1.RequestID != "r1" || len(p1.Events) != 4 || !p1.More || p1.Events[0].Kind != audit.ModeChange {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := ask(protocol.ListAuditData{Limit: 4, Before: p1.Events[3].Seq})
	if len(p2.Events) != 2 || p2.More || p2.Events[1].Seq != 1 {
		t.Fatalf("page 2: %+v", p2)
	}
	if got := ask(protocol.ListAuditData{Kinds: []string{audit.ModeChange}}); len(got.Events) != 1 {
		t.Fatalf("kinds: %+v", got)
	}
	if got := ask(protocol.ListAuditData{SessionID: "a"}); len(got.Events) != 5 {
		t.Fatalf("session: %+v", got)
	}
	if got := ask(protocol.ListAuditData{Project: "/q"}); len(got.Events) != 1 {
		t.Fatalf("project: %+v", got)
	}
	if got := ask(protocol.ListAuditData{Since: "yesterday"}); got.Error == "" || len(got.Events) != 0 {
		t.Fatalf("bad since should be an error: %+v", got)
	}
	if got := ask(protocol.ListAuditData{Since: time.Now().Add(time.Hour).Format(time.RFC3339)}); len(got.Events) != 0 {
		t.Fatalf("since: %+v", got)
	}

	// A server whose audit log could not be opened answers with an empty page.
	deps.audit = nil
	if got := ask(protocol.ListAuditData{}); got.Error != "" || got.Events == nil || len(got.Events) != 0 {
		t.Fatalf("nil log: %+v", got)
	}
}

// A device connecting and leaving is recorded under its name, on any
// transport; a wrong credential is recorded once per address per interval.
func TestAudit_PeersAndAuthFailures(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	deps.uploads, _ = NewUploadStore(t.TempDir())

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveAccepted(context.Background(), server, "unix", deps, nil)
		close(done)
	}()
	go func() { _, _ = io.Copy(io.Discard, client) }() // the server writes to us before it reads more
	_, _ = client.Write([]byte(`{"type":"auth","data":{"name":"tablet","deviceType":"web"}}` + "\n"))
	waitUntil(t, "the join", func() bool { return len(eventsOf(t, log, audit.PeerJoin)) == 1 })
	_ = client.Close()
	<-done
	joined, left := eventsOf(t, log, audit.PeerJoin), eventsOf(t, log, audit.PeerLeave)
	if len(left) != 1 || joined[0].Actor != "tablet" || left[0].Actor != "tablet" || joined[0].Network != "unix" {
		t.Fatalf("join %+v leave %+v", joined, left)
	}

	// Kicking a device names both sides.
	victim := newDevice(t, deps, "old-phone")
	laptop := newDevice(t, deps, "laptop")
	asPeer(t, deps, laptop, envelopeJSON(t, protocol.EventDisconnectPeer, protocol.DisconnectPeerData{PeerID: victim.ID}))
	if kicks := eventsOf(t, log, audit.PeerKick); len(kicks) != 1 || kicks[0].Actor != "laptop" || kicks[0].Target != "old-phone" {
		t.Fatalf("peer_kick: %+v", kicks)
	}

	// Only the first bad password from an address in the interval is logged.
	var l authFailLimiter
	now := time.Now()
	if ok, _ := l.allow("1.2.3.4", now); !ok {
		t.Fatal("first failure should be logged")
	}
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.2.3.4", now.Add(time.Second)); ok {
			t.Fatal("repeats inside the interval should be skipped")
		}
	}
	if ok, _ := l.allow("5.6.7.8", now); !ok {
		t.Fatal("another address is independent")
	}
	if ok, skipped := l.allow("1.2.3.4", now.Add(authFailLogEvery+time.Second)); !ok || skipped != 3 {
		t.Fatalf("after the interval: ok=%v skipped=%d", ok, skipped)
	}
}

// A wrong token over TCP is an auth_fail in the log, naming no device (the
// client's claimed name is not to be trusted) and never the token; a client
// that connects and leaves without presenting anything is not.
func TestAudit_BadTokenIsRecordedSilentDisconnectIsNot(t *testing.T) {
	deps, log, dir := auditedDeps(t)
	deps.uploads, _ = NewUploadStore(t.TempDir())
	authFails = authFailLimiter{} // other tests may have used the package-level limiter
	reject := func(string) (string, TokenRecord, bool) { return "", TokenRecord{}, false }

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveAccepted(context.Background(), server, "tcp", deps, reject)
		close(done)
	}()
	go func() { _, _ = io.Copy(io.Discard, client) }()
	_, _ = client.Write([]byte(`{"type":"auth","data":{"name":"mallory","token":"guess-1234567890"}}` + "\n"))
	<-done
	_ = client.Close()

	fails := eventsOf(t, log, audit.AuthFail)
	if len(fails) != 1 || fails[0].Network != "tcp" || fails[0].Outcome != "deny" || fails[0].Actor != "" {
		t.Fatalf("auth_fail: %+v", fails)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl")); strings.Contains(string(raw), "guess-1234567890") || strings.Contains(string(raw), "mallory") {
		t.Fatalf("the attempt's token or claimed name was logged:\n%s", raw)
	}

	server2, client2 := net.Pipe()
	done2 := make(chan struct{})
	go func() {
		serveAccepted(context.Background(), server2, "tcp", deps, reject)
		close(done2)
	}()
	_ = client2.Close() // a port scan
	<-done2
	if n := len(eventsOf(t, log, audit.AuthFail)); n != 1 {
		t.Fatalf("a connection that presented nothing was logged as a failed login (%d)", n)
	}
}
