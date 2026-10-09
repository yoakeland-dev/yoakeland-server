package provider

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"yoakeland/internal/agent"
)

// claudeAskTarget names a Claude Code tool call in yoakeland's rule terms so
// yoakeland's deny rules can refuse it: ("", "") when there is no equivalent.
func claudeAskTarget(toolName string, input json.RawMessage) (tool, target string) {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		URL          string `json:"url"`
	}
	_ = json.Unmarshal(input, &in)
	if strings.HasPrefix(toolName, "mcp__") { // Claude names an MCP tool the way yoakeland's rules do
		return toolName, truncate(string(input), 300)
	}
	switch toolName {
	case "Bash":
		return "bash", in.Command
	case "Write":
		return "file_write", in.FilePath
	case "Edit", "MultiEdit":
		return "file_edit", in.FilePath
	case "NotebookEdit":
		return "file_edit", in.NotebookPath
	case "WebFetch":
		if u, err := url.Parse(in.URL); err == nil && u.Hostname() != "" {
			return "web_fetch", u.Hostname()
		}
	}
	return "", ""
}

// claudeSuggestions labels Claude's own permission_suggestions (the rules
// "always allow" would add) so the user can pick one. IDs are "native:<i>".
func claudeSuggestions(raw []json.RawMessage) []agent.NativeSuggestion {
	out := make([]agent.NativeSuggestion, len(raw))
	for i, r := range raw {
		out[i] = agent.NativeSuggestion{ID: "native:" + strconv.Itoa(i), Label: claudeSuggestionLabel(r)}
	}
	return out
}

func claudeSuggestionLabel(raw json.RawMessage) string {
	var s struct {
		Type        string `json:"type"`
		Behavior    string `json:"behavior"`
		Destination string `json:"destination"`
		Mode        string `json:"mode"`
		Directories []string
		Rules       []struct {
			ToolName    string `json:"toolName"`
			RuleContent string `json:"ruleContent"`
		} `json:"rules"`
	}
	_ = json.Unmarshal(raw, &s)
	where := map[string]string{"session": " for this session", "localSettings": " in this project (local)", "projectSettings": " in this project", "userSettings": " everywhere"}[s.Destination]
	switch s.Type {
	case "addRules", "replaceRules":
		var rs []string
		for _, r := range s.Rules {
			if r.RuleContent != "" {
				rs = append(rs, r.ToolName+"("+r.RuleContent+")")
			} else {
				rs = append(rs, r.ToolName)
			}
		}
		verb := "Always allow"
		if s.Behavior == "deny" {
			verb = "Always deny"
		}
		return fmt.Sprintf("%s %s%s (Claude's rule)", verb, strings.Join(rs, ", "), where)
	case "setMode":
		return "Switch Claude to " + s.Mode + where
	case "addDirectories":
		return "Allow access to " + strings.Join(s.Directories, ", ") + where
	}
	return "Always allow (Claude's suggestion)" + where
}

// claudeChosenSuggestions is what to send back as updatedPermissions: the one
// suggestion picked, or all of them when the answer just said remember.
func claudeChosenSuggestions(raw []json.RawMessage, ruleID string) []json.RawMessage {
	if i, ok := strings.CutPrefix(ruleID, "native:"); ok {
		if n, err := strconv.Atoi(i); err == nil && n >= 0 && n < len(raw) {
			return raw[n : n+1]
		}
		return nil // a pick the agent never offered grants nothing
	}
	return raw
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
