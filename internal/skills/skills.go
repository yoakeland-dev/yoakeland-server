// Package skills finds Agent Skills (https://agentskills.io): folders holding a
// SKILL.md of instructions a model loads when a task calls for them. Every coding
// agent keeps its own skill folders and installers drop the same skill into
// several, so discovery reads them all, records which agents each can serve,
// and lets yoakeland's own agent loop use the ones meant for it.
package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Scope is whether a skill belongs to the user or to one project.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// File names the skill document.
const File = "SKILL.md"

// Limits: a skill folder is user-supplied input read into a prompt, so each
// piece of it is bounded.
const (
	maxSkills       = 500
	maxFileBytes    = 256 << 10
	maxBodyBytes    = 64 << 10
	maxDescription  = 1024
	maxListedFiles  = 100
	maxFileDepth    = 3
	maxFrontmatter  = 16 << 10
	fallbackDescLen = 300
)

// Agent names, as the provider kinds spell them ("native" is yoakeland's own loop).
const Native = "native"

// Root is a folder skills are looked for in.
type Root struct {
	Path   string
	Source string // the ecosystem it belongs to: yoake, agents, claude, codex, ...
	Scope  Scope
}

// agentsOf says which agents read a root of that source. A skill is only
// offered to an agent that would find it on its own, so a skill named to an agent
// that can't load it is never an unanswerable command.
var agentsOf = map[string][]string{
	"yoake":    {Native},
	"agents":   {Native, "codex", "amp", "opencode"},
	"claude":   {Native, "claude", "opencode"},
	"codex":    {"codex"},
	"cursor":   {"cursor"},
	"opencode": {"opencode"},
	"pi":       {"pi"},
	"omp":      {"ohmypi"},
	"amp":      {"amp"},
}

// Roots lists every folder skills may live in, in precedence order: the
// project's before the user's, and within each yoakeland's own, then the shared
// .agents, then the rest. stateDir is yoakeland's own state directory.
func Roots(stateDir, home, projectRoot string) []Root {
	var out []Root
	add := func(scope Scope, source, path string) {
		if path != "" {
			out = append(out, Root{Path: path, Source: source, Scope: scope})
		}
	}
	if projectRoot != "" {
		p := func(rel string) string { return filepath.Join(projectRoot, filepath.FromSlash(rel)) }
		add(ScopeProject, "yoake", p(".yoake/skills"))
		add(ScopeProject, "agents", p(".agents/skills"))
		add(ScopeProject, "claude", p(".claude/skills"))
		add(ScopeProject, "codex", p(".codex/skills"))
		add(ScopeProject, "opencode", p(".opencode/skills"))
		add(ScopeProject, "cursor", p(".cursor/skills"))
		add(ScopeProject, "pi", p(".pi/skills"))
		add(ScopeProject, "omp", p(".omp/skills"))
	}
	if stateDir != "" {
		add(ScopeUser, "yoake", filepath.Join(stateDir, "skills"))
	}
	if home != "" {
		h := func(rel string) string { return filepath.Join(home, filepath.FromSlash(rel)) }
		add(ScopeUser, "agents", h(".agents/skills"))
		claude := h(".claude")
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" && filepath.IsAbs(d) {
			claude = d
		}
		add(ScopeUser, "claude", filepath.Join(claude, "skills"))
		for _, d := range claudeExtraRoots(claude) {
			add(ScopeUser, "claude", d)
		}
		add(ScopeUser, "codex", h(".codex/skills"))
		add(ScopeUser, "opencode", h(".config/opencode/skills"))
		add(ScopeUser, "cursor", h(".cursor/skills"))
		add(ScopeUser, "pi", h(".pi/agent/skills"))
		add(ScopeUser, "omp", h(".omp/agent/skills"))
		add(ScopeUser, "amp", h(".config/agents/skills"))
	}
	return out
}

// Skill is one skill folder.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Dir         string   `json:"dir"`  // the real folder: symlinks resolved
	File        string   `json:"file"` // its SKILL.md
	Scope       Scope    `json:"scope"`
	Source      string   `json:"source"`
	Agents      []string `json:"agents"`
	// AlsoIn names the other roots the same folder was reached through.
	AlsoIn []string `json:"alsoIn,omitempty"`
	// Shadowed: another skill of the same name takes precedence over this one.
	Shadowed bool `json:"shadowed,omitempty"`
	// Enabled is the user's switch.
	Enabled bool `json:"enabled"`

	License       string `json:"license,omitempty"`
	Compatibility string `json:"compatibility,omitempty"`
	AllowedTools  string `json:"allowedTools,omitempty"`
	ArgumentHint  string `json:"argumentHint,omitempty"`
	// DisableModelInvocation: only the user may start it. UserInvocable false: only the model.
	DisableModelInvocation bool `json:"disableModelInvocation,omitempty"`
	UserInvocable          bool `json:"userInvocable"`

	Files    int      `json:"files"`
	Bytes    int64    `json:"bytes"`
	Modified int64    `json:"modified,omitempty"` // unix seconds
	Warnings []string `json:"warnings,omitempty"`
}

// Active: the skill is switched on and nothing hides it.
func (s Skill) Active() bool { return s.Enabled && !s.Shadowed }

// For reports whether an agent can load the skill by itself.
func (s Skill) For(agent string) bool {
	for _, a := range s.Agents {
		if a == agent {
			return true
		}
	}
	return false
}

var (
	// Lenient on purpose: real skills use capitals, dots and colons. What it
	// must be is something safe to put in a prompt and to type after $.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	// The standard's own stricter form, for a warning.
	specNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// ValidName reports whether name is usable as a skill name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// frontmatter is the keys of a SKILL.md's leading YAML block that matter here.
type frontmatter struct {
	name, description, license, compatibility, allowedTools, argumentHint string
	disableModel                                                          bool
	userInvocable                                                         bool
	body                                                                  string
	malformed                                                             bool
}

// parseFrontmatter reads the flat keys of a leading ---/--- block: scalars
// (plain, single- or double-quoted), folded and literal block scalars, and
// yes/no/true/false booleans. Nested maps (metadata:) and lists are skipped, so
// an extra key never costs a skill its listing. It is not a YAML parser, and
// doesn't pretend to be one.
func parseFrontmatter(doc string) frontmatter {
	fm := frontmatter{userInvocable: true, body: doc}
	doc = strings.TrimPrefix(doc, "\ufeff")
	if !strings.HasPrefix(doc, "---") {
		return fm
	}
	rest := doc[3:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return fm
	}
	rest = rest[nl+1:]
	var block []string
	var body string
	closed := false
	for off := 0; off < len(rest) && off < maxFrontmatter; {
		end := strings.IndexByte(rest[off:], '\n')
		line, next := rest[off:], len(rest)
		if end >= 0 {
			line, next = rest[off:off+end], off+end+1
		}
		line = strings.TrimRight(line, "\r")
		if strings.TrimRight(line, " \t") == "---" {
			closed, body = true, rest[next:]
			break
		}
		block = append(block, line)
		off = next
	}
	if !closed {
		fm.malformed = true
		return fm
	}
	fm.body = body
	for i := 0; i < len(block); i++ {
		line := block[i]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue // nested content or a comment
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if val == ">" || val == "|" || strings.HasPrefix(val, ">-") || strings.HasPrefix(val, "|-") || strings.HasPrefix(val, ">+") || strings.HasPrefix(val, "|+") {
			folded := val[0] == '>'
			var parts []string
			for i+1 < len(block) && (block[i+1] == "" || block[i+1][0] == ' ' || block[i+1][0] == '\t') {
				i++
				parts = append(parts, strings.TrimSpace(block[i]))
			}
			if folded {
				val = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			} else {
				val = strings.TrimSpace(strings.Join(parts, "\n"))
			}
		} else {
			val = unquote(val)
		}
		switch key {
		case "name":
			fm.name = val
		case "description":
			fm.description = val
		case "license":
			fm.license = val
		case "compatibility":
			fm.compatibility = val
		case "allowed-tools":
			fm.allowedTools = val
		case "argument-hint":
			fm.argumentHint = val
		case "disable-model-invocation":
			fm.disableModel = yaml11Bool(val, false)
		case "user-invocable":
			fm.userInvocable = yaml11Bool(val, true)
		}
	}
	return fm
}

func unquote(v string) string {
	if n := len(v); n >= 2 && (v[0] == '"' && v[n-1] == '"') {
		v = v[1 : n-1]
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(v)
	}
	if n := len(v); n >= 2 && v[0] == '\'' && v[n-1] == '\'' {
		return strings.ReplaceAll(v[1:n-1], "''", "'")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// yaml11Bool reads the spellings Claude Code accepts (yes/no, on/off, 1/0).
func yaml11Bool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "y", "1":
		return true
	case "false", "no", "off", "n", "0":
		return false
	}
	return def
}

func firstParagraph(body string) string {
	for _, p := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		p = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(p), "# "))
		if p != "" {
			p = strings.Join(strings.Fields(p), " ")
			if len(p) > fallbackDescLen {
				p = strings.ToValidUTF8(p[:fallbackDescLen], "") + "…"
			}
			return p
		}
	}
	return ""
}

// readSkillFile reads dir's SKILL.md through an os.Root, so a SKILL.md that is a
// symlink out of its folder is refused rather than followed.
func readSkillFile(dir string) (data []byte, modified int64, err error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	f, err := r.Open(File)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}
	if fi.Size() > maxFileBytes {
		return nil, 0, fmt.Errorf("%s is %d KB, over the %d KB limit", File, fi.Size()>>10, maxFileBytes>>10)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileBytes))
	return data, fi.ModTime().Unix(), err
}

// measure counts the files in a skill folder and their size, bounded so a
// runaway tree can't stall a scan.
func measure(dir string) (files int, bytes int64) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return 0, 0
	}
	defer r.Close()
	_ = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(d.Name(), ".") || strings.Count(p, "/") >= maxFileDepth) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if files >= maxListedFiles*5 {
			return fs.SkipAll
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	if files > 0 {
		files-- // SKILL.md itself isn't a supporting file
	}
	return files, bytes
}

// scanRoot reads every skill folder directly under root.
func scanRoot(root Root, out *[]Skill, problems *[]string) {
	entries, err := os.ReadDir(root.Path)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if len(*out) >= maxSkills {
			return
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(root.Path, name)
		// A skill folder is often a symlink into a shared checkout; follow it,
		// once, and work from where it really is.
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		fi, err := os.Stat(real)
		if err != nil || !fi.IsDir() {
			continue
		}
		data, modified, err := readSkillFile(real)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a folder with no SKILL.md is not a skill
		}
		if err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if !utf8.Valid(data) {
			*problems = append(*problems, fmt.Sprintf("%s: not valid UTF-8", filepath.Join(path, File)))
			continue
		}
		fm := parseFrontmatter(string(data))
		sk := Skill{Dir: real, File: filepath.Join(real, File), Scope: root.Scope, Source: root.Source, Agents: agentsOf[root.Source],
			Enabled: true, License: fm.license, Compatibility: fm.compatibility, AllowedTools: fm.allowedTools, ArgumentHint: fm.argumentHint,
			DisableModelInvocation: fm.disableModel, UserInvocable: fm.userInvocable, Modified: modified}
		sk.Name = fm.name
		if sk.Name == "" {
			sk.Name = name
			sk.Warnings = append(sk.Warnings, "no name in the frontmatter; using the folder name")
		}
		if !ValidName(sk.Name) {
			*problems = append(*problems, fmt.Sprintf("%s: %q isn't a usable skill name (letters, digits, . _ : -; up to 64)", filepath.Join(path, File), sk.Name))
			continue
		}
		if !specNameRe.MatchString(sk.Name) {
			sk.Warnings = append(sk.Warnings, "the name isn't in the standard's form (lowercase letters, digits, hyphens)")
		}
		if fm.name != "" && fm.name != name {
			sk.Warnings = append(sk.Warnings, fmt.Sprintf("the folder is called %s but the skill is %s", name, fm.name))
		}
		if fm.malformed {
			sk.Warnings = append(sk.Warnings, "the frontmatter has no closing ---")
		}
		sk.Description = strings.TrimSpace(fm.description)
		if sk.Description == "" {
			sk.Description = firstParagraph(fm.body)
			sk.Warnings = append(sk.Warnings, "no description; the first paragraph stands in for it")
		}
		if len(sk.Description) > maxDescription {
			sk.Description = strings.ToValidUTF8(sk.Description[:maxDescription], "") + "…"
			sk.Warnings = append(sk.Warnings, fmt.Sprintf("the description was cut to %d characters", maxDescription))
		}
		sk.Files, sk.Bytes = measure(real)
		*out = append(*out, sk)
	}
}

// Scan reads every root and returns the skills, with duplicates folded: the
// same folder reached through several roots is one skill, and when two different
// skills share a name the one from the earlier root wins.
func Scan(roots []Root, disabled func(dir string) bool) (skills []Skill, problems []string) {
	var raw []Skill
	for _, r := range roots {
		scanRoot(r, &raw, &problems)
	}
	byDir := map[string]int{}
	byName := map[string][]int{}
	for _, sk := range raw {
		if i, ok := byDir[sk.Dir]; ok {
			skills[i].AlsoIn = append(skills[i].AlsoIn, sk.Source)
			for _, a := range sk.Agents {
				if !skills[i].For(a) {
					skills[i].Agents = append(append([]string(nil), skills[i].Agents...), a)
				}
			}
			continue
		}
		sk.Enabled = disabled == nil || !disabled(sk.Dir)
		// A skill of the same name from an earlier (higher-precedence) root hides
		// this one for the agents the two share.
		for _, j := range byName[sk.Name] {
			if sharesAgent(skills[j], sk) {
				sk.Shadowed = true
				break
			}
		}
		byName[sk.Name] = append(byName[sk.Name], len(skills))
		byDir[sk.Dir] = len(skills)
		skills = append(skills, sk)
	}
	sort.SliceStable(skills, func(i, j int) bool {
		if skills[i].Scope != skills[j].Scope {
			return skills[i].Scope == ScopeProject
		}
		return strings.ToLower(skills[i].Name) < strings.ToLower(skills[j].Name)
	})
	return skills, problems
}

func sharesAgent(a, b Skill) bool {
	for _, x := range a.Agents {
		if b.For(x) {
			return true
		}
	}
	return false
}

// Doc is a skill's document, ready to hand a model.
type Doc struct {
	Body      string
	Files     []string // supporting files, relative to the skill's folder
	Truncated bool
}

// Read loads a skill's instructions (SKILL.md without its frontmatter) and lists
// the files that come with it.
func Read(sk Skill) (Doc, error) {
	data, _, err := readSkillFile(sk.Dir)
	if err != nil {
		return Doc{}, err
	}
	body := strings.TrimSpace(parseFrontmatter(string(data)).body)
	doc := Doc{Body: body}
	if len(body) > maxBodyBytes {
		doc.Body = strings.ToValidUTF8(body[:maxBodyBytes], "") + "\n\n[... the rest of SKILL.md is longer than yoakeland loads]"
		doc.Truncated = true
	}
	if r, err := os.OpenRoot(sk.Dir); err == nil {
		defer r.Close()
		_ = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != "." && (strings.HasPrefix(d.Name(), ".") || strings.Count(p, "/") >= maxFileDepth) {
					return fs.SkipDir
				}
				return nil
			}
			if p == File || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			if len(doc.Files) >= maxListedFiles {
				return fs.SkipAll
			}
			if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
				doc.Files = append(doc.Files, p)
			}
			return nil
		})
	}
	sort.Strings(doc.Files)
	return doc, nil
}

// Find returns the active skill an agent can use under name.
func Find(skills []Skill, name, agent string) (Skill, bool) {
	for _, sk := range skills {
		if sk.Name == name && sk.Active() && sk.For(agent) {
			return sk, true
		}
	}
	return Skill{}, false
}

// claudeExtraRoots finds the skills Claude Code keeps outside skills/: those of
// installed plugins (listed in plugins/installed_plugins.json) and the buckets
// of synced skills under skills/synced. A plugin's path is only trusted when it
// sits inside the Claude directory.
func claudeExtraRoots(claude string) []string {
	var out []string
	if b, err := os.ReadFile(filepath.Join(claude, "plugins", "installed_plugins.json")); err == nil {
		var f struct {
			Plugins map[string][]struct {
				InstallPath string `json:"installPath"`
			} `json:"plugins"`
		}
		if json.Unmarshal(b, &f) == nil {
			for _, list := range f.Plugins {
				for _, in := range list {
					if rel, err := filepath.Rel(claude, in.InstallPath); err == nil && filepath.IsLocal(rel) {
						out = append(out, filepath.Join(in.InstallPath, "skills"))
					}
				}
			}
		}
	}
	buckets, _ := filepath.Glob(filepath.Join(claude, "skills", "synced", "*"))
	return append(out, buckets...)
}
