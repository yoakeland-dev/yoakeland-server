package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Importing the servers other tools on this machine already have (SPEC.md
// §3.1 scan_mcp_imports). A scan only reads files and never starts anything.
// The wire sees every secret as Redacted; an import re-reads the file and
// converts the entry again, so a secret goes from that file to ours without
// crossing the connection.

const (
	maxImportFile = 256 << 10
	// ~/.claude.json also holds Claude Code's history and caches, so it grows well past a settings file.
	maxClaudeFile = 4 << 20
)

// ImportPaths names the files a scan reads. An empty path is skipped.
type ImportPaths struct {
	Claude, ClaudeDesktop, Codex, Cursor, Gemini, Kimi, Grok, OpenCode, Amp, Pi, OhMyPi string
}

// DefaultImportPaths is where each tool keeps its servers for this user.
func DefaultImportPaths(home string, getenv func(string) string) ImportPaths {
	claude := filepath.Join(home, ".claude.json")
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		claude = filepath.Join(d, ".claude.json")
	}
	var desktop string
	if dir, err := os.UserConfigDir(); err == nil {
		desktop = filepath.Join(dir, "Claude", "claude_desktop_config.json")
	}
	under := func(env, dir string) string {
		if d := getenv(env); d != "" {
			return d
		}
		return filepath.Join(home, dir)
	}
	return ImportPaths{
		Claude:        claude,
		ClaudeDesktop: desktop,
		Codex:         filepath.Join(under("CODEX_HOME", ".codex"), "config.toml"),
		Cursor:        filepath.Join(home, ".cursor", "mcp.json"),
		Gemini:        filepath.Join(home, ".gemini", "settings.json"),
		Kimi:          filepath.Join(home, ".kimi", "mcp.json"),
		Grok:          filepath.Join(under("GROK_HOME", ".grok"), "config.toml"),
		OpenCode:      filepath.Join(home, ".config", "opencode", "opencode.json"),
		Amp:           filepath.Join(home, ".config", "amp", "settings.json"),
		Pi:            filepath.Join(home, ".pi", "agent", "mcp.json"),
		OhMyPi:        filepath.Join(home, ".omp", "agent", "mcp.json"),
	}
}

// ImportCandidate is one server another tool has. Config is secret-bearing
// until Redacted is called.
type ImportCandidate struct {
	ID            string       `json:"id"`
	Source        string       `json:"source"`
	File          string       `json:"file"`
	Name          string       `json:"name"`
	SuggestedName string       `json:"suggestedName"`
	Config        ServerConfig `json:"config"`
	AlreadyAdded  bool         `json:"alreadyAdded,omitempty"`
	NameTaken     bool         `json:"nameTaken,omitempty"`
	Unsupported   string       `json:"unsupported,omitempty"`
}

// Redacted is the candidate as the wire may see it.
func (c ImportCandidate) Redacted() ImportCandidate {
	c.Config = redact(c.Config)
	return c
}

// ImportItem is what a client asks to import: a candidate id, and optionally another name.
type ImportItem struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// PlannedImport is one server ready to save.
type PlannedImport struct {
	Name, Source string
	Config       ServerConfig
}

// Importer scans and imports. It remembers what the last scan saw of each
// entry, so an import refuses one that changed since the user looked.
type Importer struct {
	paths ImportPaths
	mu    sync.Mutex
	seen  map[string]string // project + id -> config hash at the last scan
}

func NewImporter(paths ImportPaths) *Importer {
	return &Importer{paths: paths, seen: map[string]string{}}
}

// Scan lists what the other tools have, with secrets redacted. mine is the user's own servers.
func (im *Importer) Scan(project string, mine map[string]ServerConfig) (out []ImportCandidate, problems []string) {
	project = canonPath(project)
	cands, problems := scanImports(im.paths, project, mine)
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, c := range cands {
		im.seen[project+"\x00"+c.ID] = c.Config.Hash()
		out = append(out, c.Redacted())
	}
	return out, problems
}

// Plan resolves items against the files as they are now. It refuses the whole
// batch if any item can't be imported, so nothing is half done.
func (im *Importer) Plan(project string, mine map[string]ServerConfig, items []ImportItem) ([]PlannedImport, error) {
	if len(items) == 0 {
		return nil, errors.New("nothing was chosen to import")
	}
	project = canonPath(project)
	cands, _ := scanImports(im.paths, project, mine)
	byID := make(map[string]ImportCandidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	var plan []PlannedImport
	used := map[string]bool{}
	for _, it := range items {
		c, ok := byID[it.ID]
		want, scanned := im.seen[project+"\x00"+it.ID]
		if !ok || !scanned || want != c.Config.Hash() {
			return nil, fmt.Errorf("%s changed or is gone since it was listed: look again", it.ID)
		}
		if c.Unsupported != "" {
			return nil, fmt.Errorf("%s can't be imported: %s", it.ID, c.Unsupported)
		}
		name := strings.TrimSpace(it.Name)
		if name == "" {
			name = c.SuggestedName
		}
		if err := ValidName(name); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if _, taken := mine[name]; taken || used[name] {
			return nil, fmt.Errorf("there is already a server named %s", name)
		}
		used[name] = true
		plan = append(plan, PlannedImport{Name: name, Source: c.Source, Config: c.Config})
	}
	return plan, nil
}

type rawEntry struct {
	name string
	cfg  ServerConfig
	err  error
}

type importSource struct {
	name, path string
	limit      int64
	entries    func(data []byte, project string) ([]rawEntry, error)
}

// canonPath names a project the same way however a caller spelled it.
func canonPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

func scanImports(p ImportPaths, project string, mine map[string]ServerConfig) (out []ImportCandidate, problems []string) {
	project = canonPath(project)
	sources := []importSource{
		{"claude", p.Claude, maxClaudeFile, claudeEntries},
		{"claude_desktop", p.ClaudeDesktop, maxImportFile, keyedEntries("mcpServers", convertStd)},
		{"codex", p.Codex, maxImportFile, tomlEntries},
		{"cursor", p.Cursor, maxImportFile, keyedEntries("mcpServers", convertStd)},
		{"gemini", p.Gemini, maxImportFile, keyedEntries("mcpServers", convertGemini)},
		{"kimi", p.Kimi, maxImportFile, keyedEntries("mcpServers", convertStd)},
		{"grok", p.Grok, maxImportFile, tomlEntries},
		{"opencode", p.OpenCode, maxImportFile, keyedEntries("mcp", convertOpenCode)},
		{"amp", p.Amp, maxImportFile, keyedEntries("amp.mcpServers", convertStd)},
		{"pi", p.Pi, maxImportFile, keyedEntries("mcpServers", convertStd)},
		{"ohmypi", p.OhMyPi, maxImportFile, keyedEntries("mcpServers", convertStd)},
	}
	for _, s := range sources {
		if s.path == "" {
			continue
		}
		data, err := readCapped(s.path, s.limit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, s.path+": "+err.Error())
			continue
		}
		entries, err := s.entries(data, project)
		if err != nil {
			problems = append(problems, s.path+" doesn't parse: "+err.Error())
			continue
		}
		for _, e := range entries {
			c := ImportCandidate{ID: s.name + ":" + e.name, Source: s.name, File: s.path, Name: e.name, Config: e.cfg}
			switch {
			case e.err != nil:
				c.Unsupported = e.err.Error()
			default:
				if err := e.cfg.Validate(); err != nil {
					c.Unsupported = err.Error()
				}
			}
			_, c.NameTaken = mine[e.name]
			c.SuggestedName = suggestName(e.name, mine)
			c.AlreadyAdded = c.Unsupported == "" && hasSameServer(mine, e.cfg)
			out = append(out, c)
		}
	}
	return out, problems
}

// readCapped reads a regular file at most limit bytes long. A dotfile manager
// links a config into place, so a symlink is followed to the file it names, as
// long as that file is inside the user's home folder; that file is read through
// an os.Root on its own folder.
func readCapped(path string, limit int64) ([]byte, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		if !underHome(real) {
			return nil, errors.New("is a link to a file outside your home folder")
		}
		path = real
	}
	r, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("is too large to read")
	}
	return data, nil
}

// underHome reports whether path is inside the user's home folder.
func underHome(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	rel, err := filepath.Rel(home, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// suggestName makes name fit the name rule, then keeps it clear of the user's own servers.
func suggestName(name string, mine map[string]ServerConfig) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && (r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	base := b.String()
	for strings.Contains(base, "__") {
		base = strings.ReplaceAll(base, "__", "_")
	}
	base = strings.TrimLeft(base, "-_")
	if len(base) > 36 {
		base = base[:36] // room for a -N suffix
	}
	if base == "" {
		base = "server"
	}
	cand := base
	for i := 2; ; i++ {
		if _, taken := mine[cand]; !taken {
			return cand
		}
		cand = fmt.Sprintf("%s-%d", base, i)
	}
}

func hasSameServer(mine map[string]ServerConfig, c ServerConfig) bool {
	for _, m := range mine {
		if m.EffectiveType() != c.EffectiveType() {
			continue
		}
		if c.URL != "" && m.URL == c.URL {
			return true
		}
		if c.Command != "" && m.Command == c.Command && strings.Join(m.Args, "\x00") == strings.Join(c.Args, "\x00") {
			return true
		}
	}
	return false
}

// --- per-tool shapes ---

// keyedEntries reads the servers under one top-level key of a JSON file.
func keyedEntries(key string, convert func(json.RawMessage) (ServerConfig, error)) func([]byte, string) ([]rawEntry, error) {
	return func(data []byte, _ string) ([]rawEntry, error) {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, err
		}
		return convertAll(top[key], convert), nil
	}
}

// claudeEntries are ~/.claude.json's own servers plus the ones kept for this
// project; a project's entry wins over a same-named one of the user's.
func claudeEntries(data []byte, project string) ([]rawEntry, error) {
	var top struct {
		MCPServers json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	entries := convertAll(top.MCPServers, convertStd)
	if project == "" {
		return entries, nil
	}
	for path, p := range top.Projects {
		if canonPath(path) != project {
			continue
		}
		for _, e := range convertAll(p.MCPServers, convertStd) {
			replaced := false
			for i := range entries {
				if entries[i].name == e.name {
					entries[i], replaced = e, true
				}
			}
			if !replaced {
				entries = append(entries, e)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func convertAll(raw json.RawMessage, convert func(json.RawMessage) (ServerConfig, error)) []rawEntry {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]rawEntry, 0, len(names))
	for _, n := range names {
		cfg, err := convert(m[n])
		out = append(out, rawEntry{name: n, cfg: cfg, err: err})
	}
	return out
}

// convertStd reads the shape Claude Code, Claude Desktop, Cursor and Amp share
// with .mcp.json. What only the user's own config may set is cleared.
func convertStd(raw json.RawMessage) (ServerConfig, error) {
	var c ServerConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return ServerConfig{}, errors.New("not in the expected shape")
	}
	var alt struct {
		Transport string `json:"transport"` // Kimi's name for type
	}
	_ = json.Unmarshal(raw, &alt)
	if c.Type == "" {
		c.Type = alt.Transport
	}
	c.Agents, c.Disabled, c.DisabledTools = false, false, nil
	return c, nil
}

// convertOpenCode maps OpenCode's "local" (a command array) and "remote" entries.
func convertOpenCode(raw json.RawMessage) (ServerConfig, error) {
	var e struct {
		Type        string            `json:"type"`
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
		URL         string            `json:"url"`
		Headers     map[string]string `json:"headers"`
		Timeout     int               `json:"timeout"`
		OAuth       json.RawMessage   `json:"oauth"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return ServerConfig{}, errors.New("not in the expected shape")
	}
	c := ServerConfig{Env: e.Environment, Headers: e.Headers, TimeoutMs: e.Timeout}
	switch e.Type {
	case "local":
		if len(e.Command) == 0 {
			return c, errors.New("a local server needs a command")
		}
		c.Type, c.Command, c.Args = TypeStdio, e.Command[0], e.Command[1:]
	case "remote":
		c.Type, c.URL = TypeHTTP, e.URL
		var o OAuthConfig
		if json.Unmarshal(e.OAuth, &o) == nil && o.ClientID != "" {
			c.OAuth = &o
		}
	default:
		return c, fmt.Errorf("unknown transport %q", e.Type)
	}
	return c, nil
}

// convertGemini maps Gemini CLI's entries: the key that holds the address picks
// the transport (httpUrl is streamable HTTP, url is the older SSE).
func convertGemini(raw json.RawMessage) (ServerConfig, error) {
	var e struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Cwd     string            `json:"cwd"`
		URL     string            `json:"url"`
		HTTPURL string            `json:"httpUrl"`
		TCP     string            `json:"tcp"`
		Headers map[string]string `json:"headers"`
		Timeout int               `json:"timeout"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return ServerConfig{}, errors.New("not in the expected shape")
	}
	c := ServerConfig{Command: e.Command, Args: e.Args, Env: e.Env, Cwd: e.Cwd, Headers: e.Headers, TimeoutMs: e.Timeout}
	switch {
	case e.Command != "":
	case e.HTTPURL != "":
		c.Type, c.URL = TypeHTTP, e.HTTPURL
	case e.URL != "":
		c.Type, c.URL = TypeSSE, e.URL
	case e.TCP != "":
		return c, errors.New("a tcp server isn't supported")
	}
	return c, nil
}

// tomlEntries reads [mcp_servers.*] from Codex's config.toml or Grok's.
func tomlEntries(data []byte, _ string) ([]rawEntry, error) {
	var top struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(string(data), &top); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(top.Servers))
	for n := range top.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]rawEntry, 0, len(names))
	for _, n := range names {
		cfg, err := convertTOML(top.Servers[n])
		out = append(out, rawEntry{name: n, cfg: cfg, err: err})
	}
	return out, nil
}

func tomlStr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func tomlStrs(m map[string]any, k string) []string {
	var out []string
	list, _ := m[k].([]any)
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func tomlMap(m map[string]any, k string) map[string]string {
	t, _ := m[k].(map[string]any)
	var out map[string]string
	for key, v := range t {
		if s, ok := v.(string); ok {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = s
		}
	}
	return out
}

// convertTOML maps one Codex or Grok entry. Codex keeps a bearer token or a
// header's value in an environment variable it names; that becomes a ${VAR}
// reference, which is how this daemon reads a secret it doesn't hold either.
func convertTOML(m map[string]any) (ServerConfig, error) {
	c := ServerConfig{
		Command: tomlStr(m, "command"), Args: tomlStrs(m, "args"), Cwd: tomlStr(m, "cwd"), URL: tomlStr(m, "url"),
		Env: tomlMap(m, "env"), Headers: tomlMap(m, "headers"),
	}
	if c.Type = tomlStr(m, "type"); c.Type == "" {
		c.Type = tomlStr(m, "transport")
	}
	for k, v := range tomlMap(m, "http_headers") {
		if c.Headers == nil {
			c.Headers = map[string]string{}
		}
		c.Headers[k] = v
	}
	for k, name := range tomlMap(m, "env_http_headers") {
		if c.Headers == nil {
			c.Headers = map[string]string{}
		}
		c.Headers[k] = "${" + name + "}"
	}
	if v := tomlStr(m, "bearer_token_env_var"); v != "" {
		if c.Headers == nil {
			c.Headers = map[string]string{}
		}
		c.Headers["Authorization"] = "Bearer ${" + v + "}"
	}
	for _, name := range tomlStrs(m, "env_vars") {
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		if _, set := c.Env[name]; !set {
			c.Env[name] = "${" + name + "}"
		}
	}
	// A timeout this daemon would refuse is left out rather than failing the whole entry.
	var secs float64
	switch v := m["tool_timeout_sec"].(type) {
	case int64:
		secs = float64(v)
	case float64:
		secs = v
	}
	if ms := int(secs * 1000); ms > 0 && ms <= maxCallTimeout {
		c.TimeoutMs = ms
	}
	return c, nil
}
