package permission

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Scope is where a rule lives: in memory until yoakeland restarts, in the
// project's permissions.json, or in permissions.global.json for every
// project. Scope never affects precedence — a deny in any scope beats an
// allow in any other.
type Scope string

const (
	ScopeSession Scope = "session"
	ScopeProject Scope = "project"
	ScopeGlobal  Scope = "global"
	// ScopeBuiltin marks the read-only presets; never stored.
	ScopeBuiltin Scope = "builtin"
)

// Rule sources, for the UI and the audit trail.
const (
	SourceUser      = "user"      // typed into the rules editor
	SourceSuggested = "suggested" // picked from a permission_request
	SourceMigrated  = "migrated"  // converted from a v1 exact-match entry
	SourcePreset    = "preset"    // built in
)

// Rule tool names. `bash` also governs terminal_start for deny/ask (never
// allow: a terminal is a real shell), `edit` covers file_write and file_edit.
const (
	ToolBash      = "bash"
	ToolEdit      = "edit"
	ToolFileWrite = "file_write"
	ToolFileEdit  = "file_edit"
	ToolWebFetch  = "web_fetch"
	ToolWebSearch = "web_search"
	ToolBrowser   = "browser"
)

// ToolMCPPrefix starts the name of a tool an MCP server provides
// (mcp__<server>__<tool>). A rule's Tool may be one such name, or
// mcp__<server>__* for every tool of one server; its Pattern is "*", since a
// rule covers the tool, not what it is called with.
const ToolMCPPrefix = "mcp__"

// ParseMCPTool splits an MCP tool name or rule tool into its server and tool
// (or "*"), reporting whether it is well formed.
func ParseMCPTool(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, ToolMCPPrefix)
	if !found {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok || !mcpSegment(server) || (tool != "*" && !mcpSegment(tool)) {
		return "", "", false
	}
	return server, tool, true
}

func mcpSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// coversMCP: an MCP rule's tool applies to the named tool.
func coversMCP(ruleTool, tool string) bool {
	if server, name, ok := ParseMCPTool(ruleTool); ok && name == "*" {
		// A server named "foo_" also yields "mcp__foo___x"; that tool is not foo's.
		p := ToolMCPPrefix + server + "__"
		return strings.HasPrefix(tool, p) && !strings.HasPrefix(tool[len(p):], "_")
	}
	return ruleTool == tool
}

// Rule is one permission rule.
//
// Pattern by tool:
//   - bash: argv tokens. A final "*" token means "any further arguments";
//     without it the argv must match exactly. `["go","test","*"]`.
//   - edit/file_write/file_edit: one project-relative slash path: an exact
//     file, a single-segment glob (`docs/*.md`), or a subtree (`internal/**`).
//   - web_fetch/browser: one host (or, for browser evaluate, the exact script).
//   - web_search: "*", every query (the query is the tool's argument, not the rule's).
//
// Modes lists the modes an allow rule applies in (the ratchet: an allow
// granted in Edit also applies in Auto, not in Ask). Empty means every mode
// that can ask: read_write, accept_edits, auto. Allow rules never apply in
// read_only or plan. Deny and ask rules apply in every mode when Modes is empty.
type Rule struct {
	ID        string    `json:"id"`
	Tool      string    `json:"tool"`
	Pattern   []string  `json:"pattern"`
	Decision  Decision  `json:"decision"`
	Modes     []string  `json:"modes,omitempty"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Match / NotMatch are optional example inputs (a command line or a
	// path) checked when the rule is added or loaded: a rule that does not
	// behave as its examples say is rejected. Like Codex's rules files.
	Match    []string `json:"match,omitempty"`
	NotMatch []string `json:"notMatch,omitempty"`

	// Scope is filled in when rules are listed; it is implied by where the
	// rule is stored, not persisted.
	Scope Scope `json:"scope,omitempty"`
}

func newRuleID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "r_" + hex.EncodeToString(b[:])
}

// Mode ranks: how permissive each mode is. Allow rules apply from rank 1.
var modeRank = map[string]int{
	"read_only":    0,
	"plan":         0,
	"read_write":   1,
	"accept_edits": 2,
	"auto":         3,
	"autonomous":   4,
}

// RatchetModes is the modes an allow granted in mode applies in: that mode
// and every more permissive one (autonomous bypasses the engine anyway).
func RatchetModes(mode string) []string {
	r, ok := modeRank[mode]
	if !ok || r < 1 {
		return nil // unknown or non-asking mode: every asking mode
	}
	var out []string
	for _, m := range []string{"read_write", "accept_edits", "auto"} {
		if modeRank[m] >= r {
			out = append(out, m)
		}
	}
	return out
}

// appliesInMode reports whether r is in force in mode. mode "" means the
// caller did not say: deny/ask rules apply, allow rules only when unrestricted.
func (r *Rule) appliesInMode(mode string) bool {
	if r.Decision != DecisionAllow {
		return len(r.Modes) == 0 || mode == "" || slices.Contains(r.Modes, mode)
	}
	if rk, ok := modeRank[mode]; ok && rk < 1 {
		return false // allow never widens read_only / plan
	}
	if len(r.Modes) == 0 {
		return true
	}
	return mode != "" && slices.Contains(r.Modes, mode)
}

// Validate checks a rule is well formed and safe to store. It is the one
// gate every rule passes through: the editor, a picked suggestion, a loaded
// file, a migrated entry.
func (r *Rule) Validate(scope Scope) error {
	switch r.Decision {
	case DecisionAllow, DecisionAsk, DecisionDeny:
	default:
		return fmt.Errorf("unknown decision %q", r.Decision)
	}
	for _, m := range r.Modes {
		if _, ok := modeRank[m]; !ok {
			return fmt.Errorf("unknown mode %q", m)
		}
	}
	if len(r.Pattern) == 0 {
		return fmt.Errorf("empty pattern")
	}
	for _, t := range r.Pattern {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("empty pattern token")
		}
	}
	allow := r.Decision == DecisionAllow
	switch r.Tool {
	case ToolBash:
		if err := validateBashPattern(r.Pattern, allow); err != nil {
			return err
		}
	case ToolEdit, ToolFileWrite, ToolFileEdit:
		if len(r.Pattern) != 1 {
			return fmt.Errorf("a file rule takes exactly one path pattern")
		}
		if allow && scope == ScopeGlobal {
			return fmt.Errorf("a path is relative to a project, so an edit allow cannot be global")
		}
		if err := validatePathPattern(r.Pattern[0], allow); err != nil {
			return err
		}
	case ToolWebFetch, ToolBrowser:
		if len(r.Pattern) != 1 {
			return fmt.Errorf("a %s rule takes exactly one host", r.Tool)
		}
	case ToolWebSearch:
		if len(r.Pattern) != 1 || r.Pattern[0] != "*" {
			return fmt.Errorf("a web_search rule covers every query: its pattern is *")
		}
	default:
		if _, _, ok := ParseMCPTool(r.Tool); !ok {
			return fmt.Errorf("rules do not cover tool %q", r.Tool)
		}
		if len(r.Pattern) != 1 || r.Pattern[0] != "*" {
			return fmt.Errorf("an MCP tool rule covers the whole tool: its pattern is *")
		}
	}
	return r.checkExamples()
}

func validatePathPattern(p string, allow bool) error {
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return fmt.Errorf("path pattern must be relative to the project root")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path pattern must not contain ..")
		}
	}
	if p == "**" && !allow {
		return nil
	}
	body := strings.TrimSuffix(p, "/**")
	if strings.Contains(body, "**") {
		return fmt.Errorf("** is only allowed as a trailing /**")
	}
	if allow && (p == "**" || p == "*" || p == "." || p == "./**" || p == "") {
		return fmt.Errorf("that covers the whole project; use Edit mode instead")
	}
	if _, err := path.Match(body, "x"); err != nil {
		return fmt.Errorf("bad path pattern: %v", err)
	}
	return nil
}

// checkExamples runs Match/NotMatch through the same matcher a live request
// uses (allow-style strictness, ignoring Modes and the resolved-path checks).
func (r *Rule) checkExamples() error {
	for _, ex := range r.Match {
		if !r.exampleMatches(ex) {
			return fmt.Errorf("rule does not match its own example %q", ex)
		}
	}
	for _, ex := range r.NotMatch {
		if r.exampleMatches(ex) {
			return fmt.Errorf("rule wrongly matches %q, which its notMatch says it must not", ex)
		}
	}
	return nil
}

func (r *Rule) exampleMatches(ex string) bool {
	switch r.Tool {
	case ToolBash:
		return matchBashRule(r, ex)
	case ToolEdit, ToolFileWrite, ToolFileEdit:
		return matchPathPattern(r.Pattern[0], filepath.ToSlash(filepath.Clean(ex)), r.Decision != DecisionAllow)
	default:
		if _, _, ok := ParseMCPTool(r.Tool); ok {
			return coversMCP(r.Tool, ex) // an example is a tool name here
		}
		return len(r.Pattern) == 1 && r.Pattern[0] == ex
	}
}

// Text is the rule's pattern as a human types it: shell-quoted argv for bash.
func (r *Rule) Text() string {
	if _, _, ok := ParseMCPTool(r.Tool); ok {
		return r.Tool
	}
	if r.Tool != ToolBash {
		return strings.Join(r.Pattern, " ")
	}
	parts := make([]string, len(r.Pattern))
	for i, t := range r.Pattern {
		parts[i] = shellQuote(t)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]{}!#~") || s == "*" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
