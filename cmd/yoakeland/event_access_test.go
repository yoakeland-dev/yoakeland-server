package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

// tierMay reports whether a person of tier with the default caps may send t (own-chat checks aside).
func tierMay(tier string, t protocol.EventType) bool {
	acc := Access{Tier: tier, Caps: capsFromMap(tierDefaults(tier))}
	if tier == TierFull {
		acc.Caps = allCaps()
	}
	ok, _ := checkEvent(connDeps{}, acc, &protocol.Envelope{Type: t})
	return ok
}

// outboundEvents are the event constants only the server sends, plus the handshake frames that
// are read outside the dispatcher. Every other constant in internal/protocol is something a client
// can send and must have a row in eventAccess. Adding a constant fails this test until it is
// listed here or in the table.
var outboundEvents = map[string]bool{
	"side_answer": true, "message_summary": true, "steer_accepted": true, "steer_rejected": true,
	"schedule_list": true, "schedule_fired": true, "limit_action": true, "chat_files_imported": true,
	"user_message": true, "browser_request": true, "browser_frame": true, "browser_state": true,
	"mcp_servers": true, "mcp_oauth_url": true, "mcp_import_candidates": true, "skills": true, "skill_detail": true,
	"model_delta": true, "tool_call_requested": true, "permission_request": true, "tool_result": true,
	"turn_started": true, "turn_context": true, "subagent_status": true, "turn_completed": true,
	"snapshot_capturing": true, "snapshot_created": true, "snapshot_restored": true, "error": true,
	"provider_registered": true, "provider_list": true, "provider_set": true, "model_options_set": true,
	"model_roles_set": true, "context_compacted": true, "context_breakdown": true, "openrouter_oauth_url": true,
	"provider_removed": true, "model_list": true, "favorite_models_set": true, "session_list": true,
	"session_updated": true, "session_deleted": true, "directory_list": true, "snapshot_list": true,
	"cli_availability": true, "acp_registry": true, "pair_info": true, "project_opened": true, "project_list": true,
	"project_removed": true, "file_list": true, "mode_set": true, "web_tools_enabled_set": true,
	"ask_user_question_requested": true, "plan_written": true, "todo_list": true, "session_history": true,
	"peer_list": true, "remote_access_set": true, "remote_password_set": true, "server_identity": true,
	"auto_generate_title": true, "runtime_settings": true, "default_provider_set": true, "token_registered": true,
	"pairing_request": true, "pairing_pending": true, "server_restart": true, "token_list": true,
	"token_removed": true, "notification": true, "git_status": true, "pending_asks": true, "notification_list": true,
	"process_list": true, "process_signaled": true, "problem_list": true, "log_text": true, "permission_rules": true,
	"notification_dismissed": true, "notifications_read": true, "audit_list": true, "notify_prefs": true,
	"upload_progress": true, "upload_done": true, "terminal_snapshot": true, "terminal_output": true,
	"terminal_exited": true, "terminal_closed": true, "terminal_error": true, "terminal_list": true,
	"terminal_index": true, "tree_list": true, "file_content": true, "file_written": true, "fs_op_done": true,
	"files_changed": true, "git_changes": true, "file_diff": true, "media_info": true, "media_chunk": true,
	"usage_report": true, "people": true,
	// the pairing exchange runs before a connection is a peer (pairpake.go)
	"pake_reply": true, "pake_confirm": true, "pake_result": true,
}

// protocolEvents is every EventType constant declared in internal/protocol, by wire name.
func protocolEvents(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../../internal/protocol", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventType" || len(vs.Values) != 1 {
						continue
					}
					if lit, ok := vs.Values[0].(*ast.BasicLit); ok {
						wire, _ := strconv.Unquote(lit.Value)
						out[wire] = vs.Names[0].Name
					}
				}
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("found only %d event constants; the parser is looking in the wrong place", len(out))
	}
	return out
}

func TestEveryInboundEventIsClassified(t *testing.T) {
	events := protocolEvents(t)
	for wire, name := range events {
		_, inTable := eventAccess[protocol.EventType(wire)]
		switch {
		case inTable && outboundEvents[wire]:
			t.Errorf("%s (%s) is both in eventAccess and listed as outbound", name, wire)
		case !inTable && !outboundEvents[wire]:
			t.Errorf("%s (%s) is not classified: add it to eventAccess in event_access.go (or to outboundEvents if only the server sends it)", name, wire)
		}
	}
	for wire := range outboundEvents {
		if _, ok := events[wire]; !ok {
			t.Errorf("outboundEvents lists %q, which is not an event constant", wire)
		}
	}
	for wire := range eventAccess {
		if _, ok := events[string(wire)]; !ok {
			t.Errorf("eventAccess lists %q, which is not an event constant", wire)
		}
	}
}

// The tiers, as the plan's table gives them, for a few events that say a lot.
func TestEventAccessByTier(t *testing.T) {
	for _, tc := range []struct {
		event                  protocol.EventType
		limited, trusted, full bool
	}{
		{protocol.EventUserPrompt, true, true, true},
		{protocol.EventWriteFile, true, true, true},
		{protocol.EventListProcesses, true, true, true},
		{protocol.EventListTerminals, false, false, true},
		{protocol.EventBrowserView, false, false, true},
		{protocol.EventReadLog, false, true, true},
		{protocol.EventListProblems, false, true, true},
		{protocol.EventSignalProcess, false, true, true},
		{protocol.EventScheduleTurn, false, true, true},
		{protocol.EventRegisterProvider, false, true, true},
		{protocol.EventRegisterToken, false, true, true},
		{protocol.EventRemoveToken, false, true, true},
		{protocol.EventApprovePairing, false, true, true},
		{protocol.EventSetModelRoles, false, false, true},
		{protocol.EventRemoveProvider, false, false, true},
		{protocol.EventDisconnectPeer, false, false, true},
		{protocol.EventSetPeerScope, false, false, true},
		{protocol.EventSaveMCPServer, false, false, true},
		{protocol.EventTerminalOpen, false, false, true},
		{protocol.EventSetRemoteAccess, false, false, false},
		{protocol.EventType("brand_new_event"), false, false, true},
	} {
		got := [3]bool{tierMay(TierLimited, tc.event), tierMay(TierTrusted, tc.event), tierMay(TierFull, tc.event)}
		if got != [3]bool{tc.limited, tc.trusted, tc.full} {
			t.Errorf("%s: limited/trusted/full = %v, want %v/%v/%v", tc.event, got, tc.limited, tc.trusted, tc.full)
		}
	}
}

func TestRefusalNamesWhatIsMissing(t *testing.T) {
	lim := Access{Tier: TierLimited, Caps: capsFromMap(tierDefaults(TierLimited))}
	for _, tc := range []struct {
		event protocol.EventType
		want  string
	}{
		{protocol.EventReadLog, "read_log needs Trusted access"},
		{protocol.EventSaveMCPServer, "save_mcp_server needs Full access"},
		{protocol.EventListTerminals, "list_terminals isn't allowed for this person"},
	} {
		if ok, why := checkEvent(connDeps{}, lim, &protocol.Envelope{Type: tc.event}); ok || why != tc.want {
			t.Errorf("%s: ok=%v why=%q, want %q", tc.event, ok, why, tc.want)
		}
	}
}

// A device below its person never has more than the person: a Full person on a Limited device
// acts as Limited.
func TestEventAccessFollowsTheDeviceCeiling(t *testing.T) {
	deps := newTestDeps(t)
	p := addPersonWith(t, deps, TierFull, TierLimited)
	if !remoteDenied(deps, "tcp", p.ID, protocol.EventSaveMCPServer) || remoteDenied(deps, "tcp", p.ID, protocol.EventUserPrompt) {
		t.Fatal("a Full person on a Limited device wasn't held to Limited")
	}
}

func TestModeAllowed(t *testing.T) {
	def := Access{Tier: TierLimited, Caps: capsFromMap(tierDefaults(TierLimited))}
	noAuto := def
	noAuto.Caps.Autonomous = false
	readOnly := def
	readOnly.Caps.FilesEdit = false
	for _, tc := range []struct {
		name string
		acc  Access
		mode agent.Mode
		want bool
	}{
		{"default read_write", def, agent.ModeReadWrite, true},
		{"default autonomous", def, agent.ModeAutonomous, true},
		{"no autonomous cap: autonomous", noAuto, agent.ModeAutonomous, false},
		{"no autonomous cap: auto", noAuto, agent.ModeAuto, false},
		{"no autonomous cap: accept_edits", noAuto, agent.ModeAcceptEdits, true},
		{"no files_edit: read_write", readOnly, agent.ModeReadWrite, false},
		{"no files_edit: accept_edits", readOnly, agent.ModeAcceptEdits, false},
		{"no files_edit: read_only", readOnly, agent.ModeReadOnly, true},
		{"no files_edit: plan", readOnly, agent.ModePlan, true},
		{"full", Access{Tier: TierFull, Caps: allCaps()}, agent.ModeAutonomous, true},
	} {
		if got := modeAllowed(tc.acc, tc.mode); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestSetModeAndSetProviderAreChecked(t *testing.T) {
	deps := newTestDeps(t)
	if err := deps.providerStore.Register(provider.ProviderConfig{Name: "claude-cli", Kind: "cli_claude", AuthMethod: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := deps.providerStore.Register(provider.ProviderConfig{Name: "anth", Kind: "api_key", HTTPProvider: "anthropic", APIKey: "k", AuthMethod: "manual"}); err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
	p := addPersonOn(t, deps, TierLimited, "", ipc)
	deps.settings.Users[p.UserID] = User{ID: p.UserID, Tier: TierLimited, Caps: map[string]any{CapAutonomous: false}}
	send := func(typ, data string) string {
		out.Reset()
		handleEnvelope(context.Background(), ipc, deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventType(typ), SessionID: "s1", Data: json.RawMessage(data)})
		return out.String()
	}
	if got := send("set_mode", `{"mode":"autonomous"}`); !strings.Contains(got, "isn't allowed for this person") {
		t.Errorf("autonomous without the cap: %q", got)
	}
	if got := send("set_mode", `{"mode":"read_write"}`); strings.Contains(got, `"error"`) {
		t.Errorf("read_write refused: %q", got)
	}
	if got := send("set_provider", `{"providerName":"claude-cli"}`); !strings.Contains(got, "isn't allowed for this person") {
		t.Errorf("an unlisted CLI agent: %q", got)
	}
	if got := send("set_provider", `{"providerName":"anth"}`); strings.Contains(got, "isn't allowed") {
		t.Errorf("an API-key provider refused: %q", got)
	}
	deps.settings.Users[p.UserID] = User{ID: p.UserID, Tier: TierLimited, Caps: map[string]any{CapCLI: []any{"claude-cli"}}}
	if got := send("set_provider", `{"providerName":"claude-cli"}`); strings.Contains(got, "isn't allowed") {
		t.Errorf("a listed CLI agent refused: %q", got)
	}
}

func TestTrustedRegistersOnlyItsOwnAPIProviders(t *testing.T) {
	deps := newTestDeps(t)
	var out syncBuf
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
	alice := addPersonOn(t, deps, TierTrusted, "", ipc)
	bob := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{}))
	send := func(from *Peer, data string) string {
		out.Reset()
		handleEnvelope(context.Background(), ipc, deps, "tcp", from.ID, &protocol.Envelope{Type: protocol.EventRegisterProvider, Data: json.RawMessage(data)})
		return out.String()
	}
	api := `{"name":"mine","kind":"api_key","httpProvider":"openai","apiKey":"k"}`
	if got := send(alice, api); strings.Contains(got, `"error"`) {
		t.Fatalf("a new API provider was refused: %q", got)
	}
	if got := send(alice, api); strings.Contains(got, `"error"`) {
		t.Fatalf("editing their own provider was refused: %q", got)
	}
	if got := send(bob, api); !strings.Contains(got, "wasn't added by this person") {
		t.Fatalf("editing someone else's provider: %q", got)
	}
	if got := send(alice, `{"name":"sh","kind":"cli_claude","command":"/bin/sh"}`); !strings.Contains(got, "needs Full access") {
		t.Fatalf("a CLI provider: %q", got)
	}
	if got := send(alice, `{"name":"x","kind":"api_key","httpProvider":"openai","apiKey":"k","command":"/bin/sh"}`); !strings.Contains(got, "needs Full access") {
		t.Fatalf("an API provider with a command: %q", got)
	}
}

func TestListProcessesStripsCommandBelowFull(t *testing.T) {
	deps := newTestDeps(t)
	for tier, wantCmd := range map[string]bool{TierLimited: false, TierTrusted: false, TierFull: true} {
		var out syncBuf
		p := addPersonOn(t, deps, tier, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &out))
		handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &out), deps, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventListProcesses})
		got := out.String()
		if !strings.Contains(got, `"process_list"`) {
			t.Fatalf("%s: %q", tier, got)
		}
		if has := strings.Contains(got, `"command"`); has != wantCmd {
			t.Errorf("%s: command present = %v, want %v", tier, has, wantCmd)
		}
	}
}

// read_log, list_problems and signal_process follow the table now: Trusted may, Limited may not.
func TestTrustedMayReadLogAndProblems(t *testing.T) {
	deps := newTestDeps(t)
	for _, typ := range []protocol.EventType{protocol.EventReadLog, protocol.EventListProblems} {
		var out syncBuf
		tr := addPersonOn(t, deps, TierTrusted, "", protocol.NewIPCStream(bytes.NewBuffer(nil), &out))
		handleEnvelope(context.Background(), protocol.NewIPCStream(bytes.NewBuffer(nil), &out), deps, "tcp", tr.ID, &protocol.Envelope{Type: typ})
		if strings.Contains(out.String(), "needs") || strings.Contains(out.String(), "paired device") {
			t.Errorf("%s refused to Trusted: %q", typ, out.String())
		}
	}
}
