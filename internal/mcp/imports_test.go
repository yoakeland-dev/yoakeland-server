package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func importFixture(t *testing.T) (*Importer, ImportPaths, string) {
	dir, project := t.TempDir(), t.TempDir()
	p := ImportPaths{
		Claude:   filepath.Join(dir, ".claude.json"),
		Cursor:   filepath.Join(dir, "cursor.json"),
		OpenCode: filepath.Join(dir, "opencode.json"),
		Amp:      filepath.Join(dir, "amp.json"),
	}
	writeFile(t, p.Claude, `{"numStartups": 3, "mcpServers": {
		"fs": {"type":"stdio","command":"npx","args":["-y","fs"],"env":{"TOKEN":"s3cret","HOME2":"${HOME}"},"agents":true,"disabledTools":["x"]},
		"bad__name": {"command":"x"},
		"ws": {"type":"ws","url":"ws://x"}},
		"projects": {"`+project+`": {"mcpServers": {"fs": {"command":"local-fs"}, "mine": {"url":"https://a.example/mcp","headers":{"Authorization":"Bearer k"}}}}}}`)
	writeFile(t, p.Cursor, `{not json`)
	writeFile(t, p.OpenCode, `{"mcp": {"gh": {"type":"remote","url":"https://gh.example/mcp"}, "loc": {"type":"local","command":["node","a.js"],"environment":{"K":"v"}}}}`)
	return NewImporter(p), p, project
}

func TestScanRedactsAndSkipsWhatItCannotRead(t *testing.T) {
	im, p, project := importFixture(t)
	cands, problems := im.Scan(project, map[string]ServerConfig{"fs": {Command: "npx", Args: []string{"-y", "fs"}}})
	byID := map[string]ImportCandidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	if len(problems) != 1 || !strings.Contains(problems[0], p.Cursor) {
		t.Fatalf("problems = %v", problems)
	}
	if len(cands) != 6 { // claude: fs (project's), bad__name, ws, mine; opencode: gh, loc
		t.Fatalf("got %d candidates", len(cands))
	}
	if c := byID["claude:fs"]; c.Config.Command != "local-fs" || !c.NameTaken {
		t.Fatalf("the project's entry should win and the name be taken: %+v", c)
	}
	if c := byID["claude:mine"]; c.Config.Headers["Authorization"] != Redacted {
		t.Fatalf("secret leaked: %+v", c.Config)
	}
	if c := byID["claude:bad__name"]; c.SuggestedName != "bad_name" {
		t.Fatalf("suggested %q", c.SuggestedName)
	}
	if byID["claude:ws"].Unsupported == "" {
		t.Fatal("an unknown transport should be unsupported")
	}
	if c := byID["opencode:loc"]; c.Config.Command != "node" || c.Config.Args[0] != "a.js" || c.Config.Env["K"] != Redacted {
		t.Fatalf("opencode local: %+v", c.Config)
	}
	if byID["opencode:gh"].Config.EffectiveType() != TypeHTTP {
		t.Fatal("opencode remote should be http")
	}
}

func TestScanFlagsAServerTheUserAlreadyHas(t *testing.T) {
	im, _, project := importFixture(t)
	cands, _ := im.Scan("", map[string]ServerConfig{"mine2": {URL: "https://gh.example/mcp"}})
	_ = project
	for _, c := range cands {
		if c.ID == "opencode:gh" && !c.AlreadyAdded {
			t.Fatal("same URL should be flagged alreadyAdded")
		}
	}
}

func TestImportCopiesSecretsFromTheFileAndDropsAgentsAndDisabledTools(t *testing.T) {
	im, _, project := importFixture(t)
	mine := map[string]ServerConfig{}
	if _, err := im.Plan(project, mine, []ImportItem{{ID: "claude:mine"}}); err == nil {
		t.Fatal("an import without a scan should be refused")
	}
	im.Scan("", mine) // no project: claude:fs is the user-level one
	plan, err := im.Plan("", mine, []ImportItem{{ID: "claude:fs", Name: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	c := plan[0]
	if c.Name != "files" || c.Config.Env["TOKEN"] != "s3cret" || c.Config.Agents || c.Config.DisabledTools != nil {
		t.Fatalf("plan = %+v", c)
	}
}

func TestImportRefusesAChangedUnsupportedTakenOrBadName(t *testing.T) {
	im, p, project := importFixture(t)
	mine := map[string]ServerConfig{"taken": {Command: "x"}}
	im.Scan(project, mine)
	for name, items := range map[string][]ImportItem{
		"unsupported": {{ID: "claude:ws"}},
		"taken":       {{ID: "opencode:gh", Name: "taken"}},
		"bad name":    {{ID: "opencode:gh", Name: "a__b"}},
		"duplicate":   {{ID: "opencode:gh", Name: "same"}, {ID: "opencode:loc", Name: "same"}},
		"none":        nil,
	} {
		if _, err := im.Plan(project, mine, items); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	writeFile(t, p.OpenCode, `{"mcp": {"gh": {"type":"remote","url":"https://other.example/mcp"}}}`)
	if _, err := im.Plan(project, mine, []ImportItem{{ID: "opencode:gh"}}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a changed entry should be refused, got %v", err)
	}
}

func TestScanDoesNotFollowASymlinkOutOfTheFolder(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "real.json"), `{"mcpServers":{"x":{"command":"x"}}}`)
	if err := os.Symlink(filepath.Join(outside, "real.json"), filepath.Join(dir, "mcp.json")); err != nil {
		t.Skip(err)
	}
	cands, problems := NewImporter(ImportPaths{Cursor: filepath.Join(dir, "mcp.json")}).Scan("", nil)
	if len(cands) != 0 || len(problems) != 1 {
		t.Fatalf("cands=%v problems=%v", cands, problems)
	}
}

func TestScanReadsCodexGrokGeminiKimiPiAndOhMyPi(t *testing.T) {
	dir := t.TempDir()
	p := ImportPaths{
		Codex:  filepath.Join(dir, "codex.toml"),
		Grok:   filepath.Join(dir, "grok.toml"),
		Gemini: filepath.Join(dir, "gemini.json"),
		Kimi:   filepath.Join(dir, "kimi.json"),
		Pi:     filepath.Join(dir, "pi.json"),
		OhMyPi: filepath.Join(dir, "omp.json"),
	}
	writeFile(t, p.Codex, `[plugins."x"]
enabled = true
[mcp_servers.local]
command = "node"
args = ["a.js"]
env_vars = ["API_KEY"]
[mcp_servers.local.env]
K = "v"
[mcp_servers.remote]
url = "https://r.example/mcp"
bearer_token_env_var = "GH_TOKEN"
tool_timeout_sec = 90
[mcp_servers.remote.env_http_headers]
X-Org = "ORG"`)
	writeFile(t, p.Grok, `[mcp_servers.g]
url = "https://g.example/mcp"
tool_timeout_sec = 6000
[mcp_servers.g.headers]
X-Key = "secret"`)
	writeFile(t, p.Gemini, `{"theme":"x","mcpServers":{"h":{"httpUrl":"https://h.example/mcp","timeout":5000},"s":{"url":"https://s.example/sse"},"l":{"command":"uvx","args":["git"]}}}`)
	writeFile(t, p.Kimi, `{"mcpServers":{"k":{"url":"https://k.example/sse","transport":"sse"}}}`)
	writeFile(t, p.Pi, `{"mcpServers":{"p":{"command":"p","lifecycle":"lazy"}}}`)
	writeFile(t, p.OhMyPi, `{"$schema":"x","mcpServers":{"o":{"type":"stdio","command":"o"}}}`)

	cands, problems := scanImports(p, "", nil) // unredacted: what an import copies
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	by := map[string]ImportCandidate{}
	for _, c := range cands {
		if c.Unsupported != "" {
			t.Errorf("%s unsupported: %s", c.ID, c.Unsupported)
		}
		by[c.ID] = c
	}
	for _, id := range []string{"codex:local", "codex:remote", "grok:g", "gemini:h", "gemini:s", "gemini:l", "kimi:k", "pi:p", "ohmypi:o"} {
		if _, ok := by[id]; !ok {
			t.Errorf("missing %s", id)
		}
	}
	if by["codex:local"].Redacted().Config.Env["K"] != Redacted || by["codex:local"].Config.Env["K"] != "v" || by["codex:local"].Config.Env["API_KEY"] != "${API_KEY}" {
		t.Errorf("codex env = %v", by["codex:local"].Config.Env)
	}
	r := by["codex:remote"].Config
	if r.Headers["Authorization"] != "Bearer ${GH_TOKEN}" || r.Headers["X-Org"] != "${ORG}" || r.TimeoutMs != 90000 {
		t.Errorf("codex remote = %+v", r)
	}
	if by["grok:g"].Config.TimeoutMs != 0 || by["grok:g"].Config.Headers["X-Key"] != "secret" {
		t.Errorf("grok = %+v", by["grok:g"].Config)
	}
	if g := by["gemini:h"].Config; g.EffectiveType() != TypeHTTP || g.TimeoutMs != 5000 {
		t.Errorf("gemini httpUrl = %+v", g)
	}
	if by["gemini:s"].Config.EffectiveType() != TypeSSE || by["kimi:k"].Config.EffectiveType() != TypeSSE {
		t.Error("sse transports lost")
	}
}

func TestDefaultImportPathsHonourCodexAndGrokHomes(t *testing.T) {
	env := map[string]string{"CODEX_HOME": "/c", "GROK_HOME": "/g", "CLAUDE_CONFIG_DIR": "/cl"}
	p := DefaultImportPaths("/home/u", func(k string) string { return env[k] })
	if p.Codex != "/c/config.toml" || p.Grok != "/g/config.toml" || p.Claude != "/cl/.claude.json" || p.Gemini != "/home/u/.gemini/settings.json" {
		t.Fatalf("%+v", p)
	}
}

// A dotfile manager links a config into place; the file it names is the one read.
func TestReadCappedFollowsASymlinkedConfig(t *testing.T) {
	dir, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("HOME", dotfiles)
	target := filepath.Join(dotfiles, "settings.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks here")
	}
	got, err := readCapped(link, 1<<10)
	if err != nil || string(got) != "{}" {
		t.Fatalf("readCapped through a symlink = %q, %v", got, err)
	}
	if _, err := readCapped(dir, 1<<10); err == nil {
		t.Fatal("a folder is not a config file")
	}
}
