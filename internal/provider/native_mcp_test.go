package provider

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

var mcpFixture = []tool.AgentServer{
	{Name: "files", Type: "stdio", Command: "mcp-files", Args: []string{"--root", "/work"}, Env: map[string]string{"API_TOKEN": "tok-secret-123"}},
	{Name: "docs", Type: "http", URL: "https://docs.example/mcp", Headers: map[string]string{"X-Key": "hdr-secret-456"}},
	{Name: "old", Type: "sse", URL: "https://old.example/sse"},
}

func TestClaudeMCPConfig(t *testing.T) {
	if claudeMCPConfig(nil) != "" {
		t.Fatal("no servers, no config")
	}
	var got struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(claudeMCPConfig(mcpFixture)), &got); err != nil {
		t.Fatal(err)
	}
	files := got.MCPServers["files"]
	if files["type"] != "stdio" || files["command"] != "mcp-files" || files["env"].(map[string]any)["API_TOKEN"] != "tok-secret-123" {
		t.Fatalf("files = %v", files)
	}
	if d := got.MCPServers["docs"]; d["type"] != "http" || d["url"] != "https://docs.example/mcp" || d["headers"].(map[string]any)["X-Key"] != "hdr-secret-456" {
		t.Fatalf("docs = %v", d)
	}
	if got.MCPServers["old"]["type"] != "sse" {
		t.Fatalf("old = %v", got.MCPServers["old"])
	}
}

func TestClaudeIsGivenTheServersThroughAPrivateFile(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(claudeScript(0.1))}

	nt := liveTurn(key, agent.ModeReadWrite, "", "hello")
	nt.MCP = mcpFixture
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	args := f.args(0)
	if strings.Contains(args, "secret") || strings.Contains(args, "mcpServers") {
		t.Fatalf("the servers' secrets are on the command line: %s", args)
	}
	fields := strings.Fields(args)
	var path string
	for i, a := range fields {
		if a == "--mcp-config" {
			path = fields[i+1]
		}
	}
	if path == "" {
		t.Fatalf("no --mcp-config in %s", args)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("the config file is %v; it holds secrets", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "tok-secret-123") {
		t.Fatal("the file lacks the server config")
	}

	// Same servers: the process is reused. A different set: it restarts, same chat.
	nt2 := liveTurn(key, agent.ModeReadWrite, "sess-1", "again")
	nt2.MCP = mcpFixture
	if _, err := runNative(t, p, nt2); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 1 {
		t.Fatalf("same servers restarted the process: %d launches", f.launchCount())
	}
	nt3 := liveTurn(key, agent.ModeReadWrite, "sess-1", "third")
	nt3.MCP = mcpFixture[:1]
	if _, err := runNative(t, p, nt3); err != nil {
		t.Fatal(err)
	}
	if f.launchCount() != 2 || !strings.Contains(f.args(1), "--resume sess-1") {
		t.Fatalf("a changed server set must restart and resume: %d launches, %s", f.launchCount(), f.args(1))
	}
	// The first process is gone, and so is its file.
	f.waitClosed(t, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the secret file outlived its process")
}

func TestClaudeWithoutServersGetsNoFlag(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Dir: t.TempDir(), Start: f.starter(claudeScript(0.1))}
	if _, err := runNative(t, p, liveTurn(key, agent.ModeReadWrite, "", "hello")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.args(0), "--mcp-config") {
		t.Fatalf("args = %s", f.args(0))
	}
}

func TestACPMCPServersFollowTheAgentsCapabilities(t *testing.T) {
	all := acpMCPServers(mcpFixture, acpCaps{mcpHTTP: true, mcpSSE: true})
	if len(all) != 3 {
		t.Fatalf("%d servers", len(all))
	}
	stdio := all[0].(map[string]any)
	env := stdio["env"].([]map[string]string)
	if stdio["name"] != "files" || stdio["command"] != "mcp-files" || len(env) != 1 || env[0]["name"] != "API_TOKEN" || env[0]["value"] != "tok-secret-123" {
		t.Fatalf("stdio = %v", stdio)
	}
	http := all[1].(map[string]any)
	if http["type"] != "http" || http["url"] != "https://docs.example/mcp" || http["headers"].([]map[string]string)[0]["name"] != "X-Key" {
		t.Fatalf("http = %v", http)
	}
	// An agent that didn't say it takes http or sse is given only stdio servers.
	only := acpMCPServers(mcpFixture, acpCaps{})
	if len(only) != 1 || only[0].(map[string]any)["name"] != "files" {
		t.Fatalf("only = %v", only)
	}
	if got := acpMCPServers(nil, acpCaps{}); got == nil || len(got) != 0 {
		t.Fatal("no servers must be an empty list, not null: the protocol requires the field")
	}
}

func TestACPAgentIsHandedTheServersOnANewSession(t *testing.T) {
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	f := &liveFake{}
	hang := false
	base := acpScript(&hang)
	// An agent that accepts http servers.
	script := func(launch int, m map[string]any, out io.Writer) {
		if m["method"] == "initialize" {
			line(out, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]any{"http": true}}}})
			return
		}
		base(launch, m, out)
	}
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Dir: t.TempDir(), Start: f.starter(script)}
	nt := liveTurn(key, agent.ModeReadWrite, "", "hi")
	nt.MCP = mcpFixture
	if _, err := runNative(t, p, nt); err != nil {
		t.Fatal(err)
	}
	news := f.rpc("session/new")
	if len(news) != 1 {
		t.Fatalf("%d session/new", len(news))
	}
	servers := news[0]["params"].(map[string]any)["mcpServers"].([]any)
	if len(servers) != 2 { // stdio and http; the sse one is dropped, the agent didn't offer sse
		t.Fatalf("mcpServers = %v", servers)
	}
}
