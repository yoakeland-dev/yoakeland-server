package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Transport types a server config may name.
const (
	TypeStdio = "stdio"
	TypeHTTP  = "http" // Streamable HTTP
	TypeSSE   = "sse"  // the older HTTP+SSE transport
)

// ServerConfig is one MCP server as a user writes it. The shape is the one
// Claude Code's .mcp.json uses, so a project's file works unchanged; the
// fields below Timeout are yoakeland's own and ignored elsewhere.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Disabled keeps a configured server from starting.
	Disabled bool `json:"disabled,omitempty"`
	// Agents hands the server to the coding-agent CLIs too (Claude Code and
	// ACP agents; Codex, Amp and Pi manage their own), which then run it themselves. Off unless set.
	Agents bool `json:"agents,omitempty"`
	// DisabledTools are tools of the server (by its own names) not offered to the model.
	DisabledTools []string `json:"disabledTools,omitempty"`
	// TimeoutMs bounds one tool call; 0 means the default.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// OAuth names a pre-registered OAuth client, for a remote server whose
	// sign-in has no dynamic registration (GitHub's, for one).
	OAuth *OAuthConfig `json:"oauth,omitempty"`
}

// OAuthConfig is a client the user registered with the server's authorization server.
type OAuthConfig struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// Defaults and limits.
const (
	DefaultCallTimeout = 5 * 60 * 1000 // ms
	maxCallTimeout     = 60 * 60 * 1000
)

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// An HTTP field-name token (RFC 9110): no whitespace, separators or controls.
	headerRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// ValidName reports whether name can name a server. Names end up inside tool
// names (mcp__<server>__<tool>) and permission rules, so they are
// conservative, and "__" is the separator and can't appear.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("a server name is 1-40 letters, digits, - or _, starting with a letter or digit")
	}
	if strings.Contains(name, "__") {
		return fmt.Errorf("a server name can't contain a double underscore")
	}
	return nil
}

// EffectiveType is the transport the config means: its Type, or stdio for a
// command and HTTP for a bare URL.
func (c ServerConfig) EffectiveType() string {
	switch strings.ToLower(c.Type) {
	case "":
		if c.Command != "" {
			return TypeStdio
		}
		if c.URL != "" {
			return TypeHTTP
		}
		return ""
	case "streamable-http", "streamablehttp", "http":
		return TypeHTTP
	default:
		return strings.ToLower(c.Type)
	}
}

// Validate checks a config is something that can be started safely. It is the
// one gate every config passes through: the editor, a project file, a hash.
func (c ServerConfig) Validate() error {
	switch c.EffectiveType() {
	case TypeStdio:
		if strings.TrimSpace(c.Command) == "" {
			return fmt.Errorf("a stdio server needs a command")
		}
		if strings.ContainsRune(c.Command, 0) {
			return fmt.Errorf("the command has a NUL byte")
		}
		if c.URL != "" || len(c.Headers) > 0 {
			return fmt.Errorf("a stdio server has a command, not a URL or headers")
		}
	case TypeHTTP, TypeSSE:
		if c.Command != "" || len(c.Args) > 0 || len(c.Env) > 0 {
			return fmt.Errorf("a remote server has a URL, not a command")
		}
		u, err := url.Parse(strings.TrimSpace(c.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("the URL must be an http or https address")
		}
		if u.User != nil {
			return fmt.Errorf("don't put credentials in the URL; use a header")
		}
	case "":
		return fmt.Errorf("a server needs a command or a URL")
	default:
		return fmt.Errorf("unknown transport %q (stdio, http or sse)", c.Type)
	}
	for k := range c.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%q is not a valid environment variable name", k)
		}
	}
	for k, v := range c.Headers {
		if !headerRe.MatchString(k) {
			return fmt.Errorf("%q is not a valid header name", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("header %s has a line break in its value", k)
		}
	}
	if c.OAuth != nil && strings.TrimSpace(c.OAuth.ClientID) == "" {
		return fmt.Errorf("oauth needs a clientId")
	}
	if c.TimeoutMs < 0 || c.TimeoutMs > maxCallTimeout {
		return fmt.Errorf("the tool timeout must be between 0 and %d ms", maxCallTimeout)
	}
	return nil
}

// Hash identifies what a config would run, for approving a project's server:
// any change to the command, arguments, environment, URL or headers is a
// different server and asks again. The yoakeland-only fields don't count.
func (c ServerConfig) Hash() string {
	canon := struct {
		Type    string
		Command string
		Args    []string
		Env     [][2]string
		Cwd     string
		URL     string
		Headers [][2]string
		OAuth   *OAuthConfig
	}{c.EffectiveType(), c.Command, c.Args, sortedPairs(c.Env), c.Cwd, c.URL, sortedPairs(c.Headers), c.OAuth}
	b, _ := json.Marshal(canon)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func sortedPairs(m map[string]string) [][2]string {
	out := make([][2]string, 0, len(m))
	for k, v := range m {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// expandVars replaces ${VAR} and ${VAR:-default} from the environment, the
// way Claude Code's .mcp.json does, so a file can name a secret without
// holding it. An undefined variable with no default is an error rather than
// an empty string: a server started with a blank token fails in confusing ways.
func expandVars(s string, getenv func(string) (string, bool)) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := varRe.FindStringSubmatch(m)
		name, def, hasDef := parts[1], parts[3], parts[2] != ""
		if v, ok := getenv(name); ok && v != "" {
			return v
		}
		if hasDef {
			return def
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// Expanded returns the config with variables replaced.
func (c ServerConfig) Expanded(getenv func(string) (string, bool)) (ServerConfig, error) {
	var err error
	ex := func(s string) string {
		if err != nil {
			return s
		}
		var out string
		out, err = expandVars(s, getenv)
		return out
	}
	out := c
	out.Command, out.Cwd, out.URL = ex(c.Command), ex(c.Cwd), ex(c.URL)
	if c.OAuth != nil {
		o := *c.OAuth
		o.ClientID, o.ClientSecret = ex(o.ClientID), ex(o.ClientSecret)
		out.OAuth = &o
	}
	out.Args = make([]string, len(c.Args))
	for i, a := range c.Args {
		out.Args[i] = ex(a)
	}
	out.Env = make(map[string]string, len(c.Env))
	for k, v := range c.Env {
		out.Env[k] = ex(v)
	}
	out.Headers = make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		out.Headers[k] = ex(v)
	}
	return out, err
}

// baseEnv is what a stdio server inherits from the daemon: enough to find
// programs and behave, and nothing else. The daemon's own environment can hold
// API keys and the master key; a server gets only what its config names (the
// same rule the MCP SDKs apply).
func baseEnv(getenv func(string) string) []string {
	keep := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "LANGUAGE", "TMPDIR", "TZ",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	}
	if runtime.GOOS == "windows" {
		keep = append(keep, "APPDATA", "LOCALAPPDATA", "PATHEXT", "SYSTEMROOT", "SystemRoot", "COMSPEC", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "PROGRAMFILES", "PROGRAMDATA", "TEMP", "TMP", "USERNAME", "WINDIR")
	}
	var env []string
	for _, k := range keep {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() { // locale categories (LC_ALL, LC_CTYPE, ...)
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}
