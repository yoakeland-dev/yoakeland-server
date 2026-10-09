package tool

// AgentServer is an MCP server yoakeland hands to a coding-agent CLI to run
// itself (Claude Code, Codex, an ACP agent), in the vocabulary all of them
// share. Values are expanded and may hold secrets.
type AgentServer struct {
	Name    string
	Type    string // "stdio", "http" or "sse"
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
}

// AgentServerSource is a Source that can also say which of its servers a CLI
// agent should be given.
type AgentServerSource interface {
	AgentServers() []AgentServer
}
