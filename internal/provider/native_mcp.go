package provider

import (
	"encoding/json"
	"os"
	"sort"

	"yoakeland/internal/tool"
)

// MCP servers the user configured can be handed to a CLI agent, which then runs
// them itself (so its own permission flow applies to their tools, and yoakeland's
// "never" rules are honoured through the permission bridge).

// claudeMCPConfig is the document for claude --mcp-config, or "" for none. It
// is read in addition to Claude's own configuration.
func claudeMCPConfig(servers []tool.AgentServer) string {
	if len(servers) == 0 {
		return ""
	}
	out := map[string]any{}
	for _, s := range servers {
		switch s.Type {
		case "stdio":
			e := map[string]any{"type": "stdio", "command": s.Command}
			if len(s.Args) > 0 {
				e["args"] = s.Args
			}
			if len(s.Env) > 0 {
				e["env"] = s.Env
			}
			out[s.Name] = e
		default:
			e := map[string]any{"type": s.Type, "url": s.URL}
			if len(s.Headers) > 0 {
				e["headers"] = s.Headers
			}
			out[s.Name] = e
		}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": out})
	return string(b)
}

// writeSecretFile stores data in a file only this user can read, in a private
// directory, and returns its path and a function that removes both. A server's
// environment and headers hold secrets, so they never go on a command line,
// where any local user could read them.
func writeSecretFile(pattern string, data []byte) (path string, remove func(), err error) {
	dir, err := os.MkdirTemp("", "yoakeland-")
	if err != nil {
		return "", nil, err
	}
	remove = func() { _ = os.RemoveAll(dir) }
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		remove()
		return "", nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		remove()
		return "", nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		remove()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, err
	}
	return f.Name(), remove, nil
}

// acpMCPServers is the mcpServers parameter of session/new, load and resume.
// Every ACP agent takes stdio servers; http and sse only when it said so in
// initialize (mcpCapabilities).
func acpMCPServers(servers []tool.AgentServer, caps acpCaps) []any {
	out := []any{}
	for _, s := range servers {
		switch {
		case s.Type == "stdio":
			env := []map[string]string{}
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				env = append(env, map[string]string{"name": k, "value": s.Env[k]})
			}
			args := s.Args
			if args == nil {
				args = []string{}
			}
			out = append(out, map[string]any{"name": s.Name, "command": s.Command, "args": args, "env": env})
		case (s.Type == "http" && caps.mcpHTTP) || (s.Type == "sse" && caps.mcpSSE):
			headers := []map[string]string{}
			keys := make([]string, 0, len(s.Headers))
			for k := range s.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				headers = append(headers, map[string]string{"name": k, "value": s.Headers[k]})
			}
			out = append(out, map[string]any{"type": s.Type, "name": s.Name, "url": s.URL, "headers": headers})
		}
	}
	return out
}
