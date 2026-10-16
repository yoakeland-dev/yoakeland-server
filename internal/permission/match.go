package permission

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"yoakeland/internal/normalizer"
)

// parseArgv is ParseShellArgv (which needs no project root): the same no-shell
// tokenizer the bash tool runs a command through.
func parseArgv(cmd string) (*normalizer.ShellCommand, error) {
	return (&normalizer.Normalizer{}).ParseShellArgv(cmd)
}

// matchArgvPattern: pat is argv tokens with an optional final "*" meaning
// any further arguments. baseArgv0 compares only the program's basename, so a
// deny on `rm` also catches /bin/rm; allow rules never do that.
func matchArgvPattern(pat, argv []string, baseArgv0 bool) bool {
	wild := len(pat) > 0 && pat[len(pat)-1] == "*"
	fixed := pat
	if wild {
		fixed = pat[:len(pat)-1]
	}
	if wild {
		if len(argv) < len(fixed) {
			return false
		}
	} else if len(argv) != len(fixed) {
		return false
	}
	for i, t := range fixed {
		if i == 0 && baseArgv0 {
			if filepath.Base(argv[0]) != filepath.Base(t) {
				return false
			}
			continue
		}
		if argv[i] != t {
			return false
		}
	}
	return true
}

// matchBashRule matches a whole command line against a bash rule, with the
// strictness of the rule's decision. Used for examples and by the engine.
func matchBashRule(r *Rule, cmd string) bool {
	if r.Decision == DecisionAllow {
		sc, err := parseArgv(cmd)
		return err == nil && allowBashMatches(r, sc)
	}
	for _, c := range commandCandidates(cmd) {
		if matchArgvPattern(r.Pattern, c, true) {
			return true
		}
	}
	return false
}

// allowBashMatches: an allow needs the command to be one plain argv (no
// chaining characters) that the pattern matches token for token, program
// path included. Presets additionally refuse the flags that make an
// otherwise read-only command write or run code.
func allowBashMatches(r *Rule, sc *normalizer.ShellCommand) bool {
	if sc.HasChain || len(sc.Argv) == 0 || !matchArgvPattern(r.Pattern, sc.Argv, false) {
		return false
	}
	return r.Source != SourcePreset || presetArgsSafe(sc.Argv)
}

// Deny and ask rules must hold up against the ways a command is spelled
// differently: /bin/rm, `env rm`, `git -c x=y push`, `sh -c 'rm x'`,
// `find -exec rm`, `a; rm b`. commandCandidates expands a command line into
// every argv that could be what actually runs, and a rule fires if it matches
// any. Best effort, not a boundary: the sandbox is the boundary.
func commandCandidates(cmd string) [][]string {
	var out [][]string
	var walk func(cmd string, depth int)
	var expand func(argv []string, depth int)
	walk = func(cmd string, depth int) {
		if depth > 4 {
			return
		}
		for _, part := range splitShell(cmd) {
			if argv := parseLoose(part); len(argv) > 0 {
				expand(argv, depth)
			}
		}
	}
	expand = func(argv []string, depth int) {
		if len(out) > 96 || len(argv) == 0 {
			return
		}
		out = append(out, argv)
		switch base := filepath.Base(argv[0]); {
		case base == "git":
			if s := stripGitGlobals(argv); len(s) != len(argv) {
				expand(s, depth)
			}
		case shellNames[base]:
			for i := 1; i < len(argv); i++ {
				a := argv[i]
				if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(argv) {
					walk(argv[i+1], depth+1)
					break
				}
				if !strings.HasPrefix(a, "-") {
					break
				}
			}
		case base == "eval":
			walk(strings.Join(argv[1:], " "), depth+1)
		case base == "find":
			for i, a := range argv {
				if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
					inner := argv[i+1:]
					for j, t := range inner {
						if t == ";" || t == "+" || t == `\;` {
							inner = inner[:j]
							break
						}
					}
					expand(inner, depth+1)
				}
			}
		case wrapperNames[base]:
			// Which argument starts the wrapped command depends on the
			// wrapper's own options, so try every suffix.
			for j := 1; j < len(argv); j++ {
				expand(argv[j:], depth+1)
			}
		}
	}
	walk(cmd, 0)
	return out
}

var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true, "su": true, "ash": true}

var wrapperNames = map[string]bool{
	"env": true, "nice": true, "nohup": true, "time": true, "timeout": true, "command": true,
	"builtin": true, "exec": true, "sudo": true, "doas": true, "stdbuf": true, "setsid": true,
	"ionice": true, "xargs": true, "watch": true, "flock": true, "chroot": true, "busybox": true,
	"unshare": true, "runuser": true, "script": true,
}

// stripGitGlobals drops git's options that come before the subcommand
// (`git -c k=v -C dir push` -> `git push`).
func stripGitGlobals(argv []string) []string {
	i := 1
	for i < len(argv) && strings.HasPrefix(argv[i], "-") {
		a := argv[i]
		switch a {
		case "-c", "-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--exec-path":
			i += 2
		default:
			i++
		}
	}
	if i > len(argv) {
		i = len(argv)
	}
	return append([]string{argv[0]}, argv[i:]...)
}

// parseLoose tokenizes one command; an unbalanced quote falls back to plain
// whitespace splitting so a deny rule still sees the words.
func parseLoose(part string) []string {
	if sc, err := parseArgv(part); err == nil {
		return sc.Argv
	}
	return strings.Fields(part)
}

// splitShell cuts a command line at the places a real shell would start a
// new command: ; | & newline, ( ), backticks and $(.
func splitShell(s string) []string {
	var parts []string
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}
	inS, inD, esc := false, false, false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && !inS:
			esc = true
			cur.WriteRune(r)
		case r == '\'' && !inD:
			inS = !inS
			cur.WriteRune(r)
		case r == '"' && !inS:
			inD = !inD
			cur.WriteRune(r)
		case !inS && (r == '`' || (r == '$' && i+1 < len(rs) && rs[i+1] == '(')):
			flush()
			if r == '$' {
				i++
			}
		case !inS && !inD && (r == ';' || r == '|' || r == '&' || r == '\n' || r == '(' || r == ')'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return parts
}

// --- what an allow rule may not be ---

var noWildcardArgv0 = map[string]bool{
	// shells, wrappers, interpreters: a wildcard after them runs anything
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true, "ash": true,
	"env": true, "xargs": true, "find": true, "sudo": true, "doas": true, "su": true, "nice": true, "nohup": true,
	"time": true, "timeout": true, "stdbuf": true, "watch": true, "setsid": true, "ionice": true, "flock": true,
	"command": true, "builtin": true, "exec": true, "eval": true, "chroot": true, "busybox": true, "unshare": true,
	"node": true, "nodejs": true, "deno": true, "ruby": true, "perl": true, "php": true, "lua": true,
	"awk": true, "gawk": true, "mawk": true, "sed": true, "npx": true, "pnpx": true, "tar": true, "cpio": true,
	"ssh": true, "scp": true, "rsync": true, "socat": true, "nc": true, "ncat": true, "telnet": true,
	// destructive or network commands never get a blanket allow
	"rm": true, "rmdir": true, "mv": true, "dd": true, "chmod": true, "chown": true, "chgrp": true, "kill": true,
	"pkill": true, "killall": true, "shred": true, "truncate": true, "ln": true, "curl": true, "wget": true,
	"mkfs": true, "shutdown": true, "reboot": true, "poweroff": true,
}

// A wildcard rule for these must pin a subcommand (`git status *`, not `git *`).
var subcommandTools = map[string]bool{
	"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true, "bun": true, "cargo": true, "make": true,
	"just": true, "docker": true, "podman": true, "kubectl": true, "gh": true, "pip": true, "pip3": true,
	"uv": true, "poetry": true, "dotnet": true, "mvn": true, "gradle": true, "rake": true, "bundle": true,
	"composer": true, "terraform": true, "helm": true,
}

var (
	gitBlocked       = map[string]bool{"config": true, "submodule": true, "rebase": true, "bisect": true, "difftool": true, "mergetool": true, "filter-branch": true, "daemon": true, "instaweb": true, "alias": true}
	pkgRunners       = map[string]bool{"npm": true, "pnpm": true, "yarn": true, "bun": true}
	pkgBlocked       = map[string]bool{"exec": true, "x": true, "dlx": true, "create": true, "init": true, "npx": true}
	containerTools   = map[string]bool{"docker": true, "podman": true, "kubectl": true, "helm": true}
	containerAllowed = map[string]bool{"ps": true, "images": true, "logs": true, "inspect": true, "version": true, "info": true, "get": true, "describe": true, "list": true}
	execFlags        = map[string]bool{"-c": true, "-e": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "--exec": true, "--eval": true, "-eval": true, "-x": true, "--command": true, "--": true}
)

// unsafeWildcard says why tokens (the part before a trailing *) must not
// be given a wildcard allow, or "" if it may.
func unsafeWildcard(tokens []string) string {
	if len(tokens) == 0 {
		return "a bare * would allow every command"
	}
	first := tokens[0]
	if strings.HasPrefix(first, "-") || strings.Contains(first, "=") {
		return "the program name must come first"
	}
	base := filepath.Base(first)
	if noWildcardArgv0[base] || strings.HasPrefix(base, "python") {
		return base + " runs whatever it is given (or is destructive); allow the exact command instead"
	}
	for _, t := range tokens[1:] {
		if execFlags[t] {
			return "an argument like " + t + " runs arbitrary commands"
		}
	}
	if !subcommandTools[base] {
		return ""
	}
	if len(tokens) < 2 || strings.HasPrefix(tokens[1], "-") {
		return base + " needs a subcommand in the rule (e.g. `" + base + " status *`)"
	}
	sub := tokens[1]
	switch {
	case base == "git" && gitBlocked[sub]:
		return "git " + sub + " can run arbitrary programs"
	case pkgRunners[base] && pkgBlocked[sub]:
		return base + " " + sub + " runs arbitrary packages"
	case pkgRunners[base] && sub == "run" && len(tokens) < 3:
		return "name the script: `" + base + " run test *`"
	case containerTools[base] && !containerAllowed[sub]:
		return base + " " + sub + " can run arbitrary commands"
	}
	return ""
}

func validateBashPattern(pat []string, allow bool) error {
	wild := pat[len(pat)-1] == "*"
	for i, t := range pat {
		if t == "*" && i != len(pat)-1 {
			return fmt.Errorf("* is only a wildcard as the last token")
		}
	}
	if !allow {
		return nil
	}
	if wild {
		if why := unsafeWildcard(pat[:len(pat)-1]); why != "" {
			return fmt.Errorf("too broad to allow: %s", why)
		}
	}
	return nil
}

// --- read-only presets ---

var presetRejectPrefixes = []string{"--output", "--ext-diff", "--no-index", "--textconv", "--open-files-in-pager", "--exec-path", "--git-dir", "--work-tree", "--config-env", "--pager"}

// presetArgsSafe refuses the flags that turn a read-only command into a write
// or into running a configured program: git's --output=, --ext-diff,
// --no-index (reads outside the project), and -c/-C/-O.
func presetArgsSafe(argv []string) bool {
	isGit := filepath.Base(argv[0]) == "git"
	for _, a := range argv[1:] {
		if isGit && !strings.HasPrefix(a, "--") && (strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-O")) {
			return false
		}
		for _, p := range presetRejectPrefixes {
			if strings.HasPrefix(a, p) {
				return false
			}
		}
	}
	return true
}

func presetRules() []Rule {
	mk := func(id string, pat ...string) Rule {
		return Rule{ID: "preset:" + id, Tool: ToolBash, Pattern: pat, Decision: DecisionAllow, Source: SourcePreset, Scope: ScopeBuiltin}
	}
	return []Rule{
		mk("ls", "ls", "*"),
		mk("pwd", "pwd"),
		mk("git-status", "git", "status", "*"),
		mk("git-diff", "git", "diff", "*"),
		mk("git-log", "git", "log", "*"),
		mk("git-show", "git", "show", "*"),
	}
}

// --- paths ---

// matchPathPattern matches a slash path (project-relative) against a rule's
// path pattern: an exact file, a single-segment glob, or `dir/**` for a whole
// subtree. deny compares case-insensitively so .ENV cannot dodge a .env rule.
func matchPathPattern(pat, rel string, deny bool) bool {
	if deny {
		pat, rel = strings.ToLower(pat), strings.ToLower(rel)
	}
	pat = strings.TrimPrefix(pat, "./")
	rel = strings.TrimPrefix(rel, "./")
	if pat == "**" {
		return true
	}
	if dir, ok := strings.CutSuffix(pat, "/**"); ok {
		segs := strings.Split(rel, "/")
		for i := range segs {
			if m, _ := path.Match(dir, strings.Join(segs[:i+1], "/")); m {
				return true
			}
		}
		return false
	}
	m, _ := path.Match(pat, rel)
	return m
}

// relSlash is target relative to root as a slash path, or "" if it is not
// inside root.
func relSlash(root, target string) string {
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			return ""
		}
		target = rel
	}
	target = filepath.Clean(target)
	if target == ".." || strings.HasPrefix(target, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(target)
}

// secretPath: files that usually hold credentials. A read of one asks
// whatever rules say, and a search skips them unless asked for by name.
func secretPath(p string) bool {
	segs := strings.Split(strings.ToLower(filepath.ToSlash(filepath.Clean(p))), "/")
	last := segs[len(segs)-1]
	switch {
	case strings.HasPrefix(last, ".env"), strings.HasPrefix(last, "id_rsa"), last == ".npmrc", last == ".netrc",
		strings.HasSuffix(last, ".pem"), strings.HasSuffix(last, ".key"):
		return true
	}
	return len(segs) >= 2 && segs[len(segs)-2] == ".git" && last == "config"
}

// neverAllow: paths no allow rule may ever cover — git's own directory (its
// config and hooks are code git runs later), and .env* files (secrets).
func neverAllow(norm *normalizer.Normalizer, resolved string) bool {
	if norm.IsGitExecPath(resolved) {
		return true
	}
	rel := relSlash(norm.ProjectRoot, resolved)
	if rel == "" {
		return true
	}
	for _, seg := range strings.Split(strings.ToLower(rel), "/") {
		if seg == ".git" || strings.HasPrefix(seg, ".env") {
			return true
		}
	}
	return false
}
