package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yoakeland/internal/mcp"
	"yoakeland/internal/protocol"
)

// TestMain turns the test binary into a one-tool stdio MCP server when asked,
// so the dispatch tests have a real server process to talk to.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_STDIO") != "" {
		sc := bufio.NewScanner(os.Stdin)
		var mu sync.Mutex
		send := func(v any) {
			b, _ := json.Marshal(v)
			mu.Lock()
			fmt.Fprintf(os.Stdout, "%s\n", b)
			mu.Unlock()
		}
		for sc.Scan() {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
				continue
			}
			var result any
			switch m.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "dispatch-fake", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "Say pong.", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
			default:
				result = map[string]any{}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "result": result})
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServerConfig() mcp.ServerConfig {
	return mcp.ServerConfig{Command: os.Args[0], Env: map[string]string{"MCP_FAKE_STDIO": "1", "SECRET_TOKEN": "s3cr3t-value"}}
}

func mcpReply(t *testing.T, envs []protocol.Envelope, i int) mcpServersPayload {
	t.Helper()
	if i >= len(envs) || envs[i].Type != protocol.EventMCPServers {
		t.Fatalf("reply %d is not mcp_servers: %+v", i, envs)
	}
	var d mcpServersPayload
	if err := json.Unmarshal(envs[i].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitState(t *testing.T, pc *ProjectContext, name string, want mcp.State) mcp.ServerStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		servers, _ := pc.MCP.Status()
		for _, s := range servers {
			if s.Name == name && s.State == want {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	servers, _ := pc.MCP.Status()
	t.Fatalf("server %s never reached %s: %+v", name, want, servers)
	return mcp.ServerStatus{}
}

func TestServeConnection_MCPServerLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := newTestDeps(t)
	t.Cleanup(deps.projects.CloseMCP)
	pc, _ := deps.projects.Get(deps.defaultProjectRoot)

	cfg := fakeServerConfig()
	envs := runDispatch(t, deps,
		envelopeJSON(t, protocol.EventListMCPServers, map[string]any{}),
		envelopeJSON(t, protocol.EventSaveMCPServer, map[string]any{"name": "fake", "config": cfg}),
		envelopeJSON(t, protocol.EventSaveMCPServer, map[string]any{"name": "bad name", "config": cfg}),
		envelopeJSON(t, protocol.EventSaveMCPServer, map[string]any{"name": "nocmd", "config": map[string]any{}}),
		envelopeJSON(t, protocol.EventSaveMCPServer, map[string]any{"name": "fake", "config": cfg}), // a duplicate
	)
	if r := mcpReply(t, envs, 0); r.Error != "" || len(r.Servers) != 0 || r.Redacted != mcp.Redacted {
		t.Fatalf("empty list = %+v", r)
	}
	saved := mcpReply(t, envs, 1)
	if saved.Error != "" || len(saved.Servers) != 1 || saved.Servers[0].Name != "fake" {
		t.Fatalf("save = %+v", saved)
	}
	for i := 2; i <= 4; i++ {
		if r := mcpReply(t, envs, i); r.Error == "" {
			t.Errorf("reply %d should be an error: %+v", i, r)
		}
	}
	// The secret never goes back out, not in the reply, and not in the list.
	raw, _ := json.Marshal(envs)
	if strings.Contains(string(raw), "s3cr3t-value") {
		t.Fatal("a secret environment value was sent to the client")
	}

	// The server really starts, lists its tool, and offers it to the agents.
	st := waitState(t, pc, "fake", mcp.StateReady)
	if len(st.Tools) != 1 || st.Tools[0].Name != "mcp__fake__ping" || st.Info == nil || st.Info.Name != "dispatch-fake" {
		t.Fatalf("status = %+v", st)
	}
	if st.Config.Env["SECRET_TOKEN"] != mcp.Redacted {
		t.Fatalf("env as listed = %v", st.Config.Env)
	}

	// Switch the tool off, then the server; remove it.
	envs = runDispatch(t, deps,
		envelopeJSON(t, protocol.EventSetMCPToolEnabled, map[string]any{"name": "fake", "tool": "ping", "enabled": false}),
		envelopeJSON(t, protocol.EventSetMCPServerEnabled, map[string]any{"name": "fake", "enabled": false}),
		envelopeJSON(t, protocol.EventSetMCPServerEnabled, map[string]any{"name": "fake"}), // enabled missing
		envelopeJSON(t, protocol.EventReconnectMCPServer, map[string]any{"name": "fake"}),  // refused while disabled
		envelopeJSON(t, protocol.EventRemoveMCPServer, map[string]any{"name": "fake"}),
	)
	if r := mcpReply(t, envs, 0); r.Error != "" || r.Servers[0].Tools[0].Enabled {
		t.Fatalf("tool switch = %+v", r)
	}
	if r := mcpReply(t, envs, 1); r.Servers[0].State != mcp.StateDisabled {
		t.Fatalf("server switch = %+v", r.Servers[0])
	}
	if mcpReply(t, envs, 2).Error == "" || mcpReply(t, envs, 3).Error == "" {
		t.Fatal("a missing flag and a reconnect of a disabled server must be refused")
	}
	if r := mcpReply(t, envs, 4); r.Error != "" || len(r.Servers) != 0 {
		t.Fatalf("remove = %+v", r)
	}
}

func TestServeConnection_ProjectMCPServersNeedApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := newTestDeps(t)
	t.Cleanup(deps.projects.CloseMCP)
	pc, _ := deps.projects.Get(deps.defaultProjectRoot)
	file, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"repo": fakeServerConfig()}})
	if err := os.WriteFile(filepath.Join(deps.defaultProjectRoot, ".mcp.json"), file, 0o644); err != nil {
		t.Fatal(err)
	}

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListMCPServers, map[string]any{"connect": true}))
	r := mcpReply(t, envs, 0)
	if len(r.Servers) != 1 || r.Servers[0].State != mcp.StatePending || r.Servers[0].Approval != "pending" || r.Servers[0].Scope != mcp.ScopeProject {
		t.Fatalf("a repository's server must wait for approval: %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	if st, _ := pc.MCP.Status(); st[0].State != mcp.StatePending {
		t.Fatal("connect:true started an unapproved project server")
	}

	envs = runDispatch(t, deps, envelopeJSON(t, protocol.EventSetMCPServerEnabled, map[string]any{"name": "repo", "enabled": true}))
	if r := mcpReply(t, envs, 0); r.Error != "" || r.Servers[0].Approval != "approved" {
		t.Fatalf("approve = %+v", r)
	}
	waitState(t, pc, "repo", mcp.StateReady)

	// Editing the file makes it a different server: it stops and asks again.
	cfg := fakeServerConfig()
	cfg.Args = []string{"-test.run=NoSuchTest"}
	file, _ = json.Marshal(map[string]any{"mcpServers": map[string]any{"repo": cfg}})
	_ = os.WriteFile(filepath.Join(deps.defaultProjectRoot, ".mcp.json"), file, 0o644)
	envs = runDispatch(t, deps, envelopeJSON(t, protocol.EventListMCPServers, map[string]any{}))
	if r := mcpReply(t, envs, 0); r.Servers[0].Approval != "changed" || r.Servers[0].State != mcp.StatePending {
		t.Fatalf("after editing the file: %+v", r.Servers[0])
	}
}

func TestServeConnection_MCPOAuthRefusesNonLocalCallbacks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := newTestDeps(t)
	t.Cleanup(deps.projects.CloseMCP)
	envs := runDispatch(t, deps,
		envelopeJSON(t, protocol.EventSaveMCPServer, map[string]any{"name": "remote", "config": map[string]any{"url": "https://mcp.example.invalid/mcp"}}),
		envelopeJSON(t, protocol.EventMCPOAuthStart, map[string]any{"name": "remote", "callbackUrl": "https://evil.example/cb"}),
		envelopeJSON(t, protocol.EventMCPOAuthStart, map[string]any{"name": "nope", "callbackUrl": "http://localhost:1/cb"}),
		envelopeJSON(t, protocol.EventMCPOAuthComplete, map[string]any{"requestId": "unknown", "code": "x", "state": "y"}),
	)
	for i := 1; i <= 3; i++ {
		if r := mcpReply(t, envs, i); r.Error == "" {
			t.Errorf("reply %d should be an error: %+v", i, r)
		}
	}
}

// --- skills ---

func skillsReply(t *testing.T, envs []protocol.Envelope, i int) skillsPayload {
	t.Helper()
	if i >= len(envs) || envs[i].Type != protocol.EventSkills {
		t.Fatalf("reply %d is not skills: %+v", i, envs)
	}
	var d skillsPayload
	if err := json.Unmarshal(envs[i].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestServeConnection_SkillsListSwitchAndRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	deps := newTestDeps(t)
	dir := filepath.Join(deps.defaultProjectRoot, ".agents", "skills", "release")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: release\ndescription: Cut a release\n---\nRun scripts/tag.sh."), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scripts", "tag.sh"), []byte("#!/bin/sh"), 0o755)

	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListSkills, map[string]any{}))
	r := skillsReply(t, envs, 0)
	if r.Error != "" || len(r.Skills) != 1 || r.Skills[0].Name != "release" || r.Skills[0].Scope != "project" || !r.Skills[0].Enabled {
		t.Fatalf("list = %+v", r)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if r.Skills[0].Dir != real {
		t.Fatalf("dir = %s, want %s", r.Skills[0].Dir, real)
	}
	if len(r.Roots) < 5 {
		t.Fatalf("roots = %+v (the page lists every place skills are looked for)", r.Roots)
	}

	envs = runDispatch(t, deps,
		envelopeJSON(t, protocol.EventGetSkill, map[string]any{"dir": real}),
		envelopeJSON(t, protocol.EventGetSkill, map[string]any{"dir": "/etc"}),
		envelopeJSON(t, protocol.EventSetSkillEnabled, map[string]any{"dir": real, "enabled": false}),
		envelopeJSON(t, protocol.EventSetSkillEnabled, map[string]any{"dir": "/etc", "enabled": false}),
		envelopeJSON(t, protocol.EventSetSkillEnabled, map[string]any{"dir": real}),
	)
	var detail skillDetailPayload
	_ = json.Unmarshal(envs[0].Data, &detail)
	if envs[0].Type != protocol.EventSkillDetail || detail.Body != "Run scripts/tag.sh." || len(detail.Files) != 1 || detail.Files[0] != "scripts/tag.sh" {
		t.Fatalf("detail = %+v", detail)
	}
	_ = json.Unmarshal(envs[1].Data, &detail)
	if detail.Error == "" {
		t.Fatal("reading a folder that isn't a discovered skill must fail")
	}
	if r := skillsReply(t, envs, 2); r.Error != "" || r.Skills[0].Enabled {
		t.Fatalf("switch off = %+v", r)
	}
	if skillsReply(t, envs, 3).Error == "" || skillsReply(t, envs, 4).Error == "" {
		t.Fatal("switching a path that isn't a skill, or omitting the flag, must fail")
	}
	// What the agent sees follows the switch.
	ps := projectSkills{cat: deps.projects.ext.skills, root: deps.defaultProjectRoot}
	canon, _ := filepath.EvalSymlinks(deps.defaultProjectRoot)
	ps.root = canon
	if got := ps.Skills("native"); len(got) != 0 {
		t.Fatalf("a disabled skill is still offered to the agent: %+v", got)
	}
	if _, err := ps.LoadSkill("release", false); err == nil {
		t.Fatal("a disabled skill must not load")
	}
}

func TestProjectSkillsLoadRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := newTestDeps(t)
	root, _ := filepath.EvalSymlinks(deps.defaultProjectRoot)
	mk := func(name, front string) {
		d := filepath.Join(root, ".agents", "skills", name)
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n"+front+"---\nBODY-"+name), 0o644)
	}
	mk("open", "")
	mk("useronly", "disable-model-invocation: true\n")
	mk("modelonly", "user-invocable: false\n")
	ps := projectSkills{cat: deps.projects.ext.skills, root: root}
	if doc, err := ps.LoadSkill("open", false); err != nil || doc.Body != "BODY-open" {
		t.Fatalf("open = %+v, %v", doc, err)
	}
	if _, err := ps.LoadSkill("useronly", false); err == nil {
		t.Fatal("the model may not load a user-only skill")
	}
	if doc, err := ps.LoadSkill("useronly", true); err != nil || doc.Body != "BODY-useronly" {
		t.Fatalf("the user may: %+v, %v", doc, err)
	}
	if _, err := ps.LoadSkill("modelonly", true); err == nil {
		t.Fatal("a skill that isn't user-invocable can't be started by hand")
	}
	if _, err := ps.LoadSkill("modelonly", false); err != nil {
		t.Fatalf("the model may load it: %v", err)
	}
	if _, err := ps.LoadSkill("ghost", false); err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("an unknown skill should say what exists: %v", err)
	}
}
