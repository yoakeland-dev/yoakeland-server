package permission

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yoakeland/internal/normalizer"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// ErrRulesCorrupt: a rules file could not be loaded, so nothing is written
// to it and no allow rule is honoured until it is fixed.
var ErrRulesCorrupt = errors.New("permission rules file could not be loaded")

// RuleSet is one scope's rules: the session's (memory only), a project's or
// the global file.
type RuleSet struct {
	mu      sync.RWMutex
	rules   []Rule
	store   *PersistentStore // nil for the session
	scope   Scope
	loadErr error
	notes   []string
}

func newRuleSet(scope Scope, store *PersistentStore, root string) *RuleSet {
	rs := &RuleSet{scope: scope, store: store}
	if store != nil {
		rs.rules, rs.notes, rs.loadErr = store.Load(root)
	}
	return rs
}

var (
	globalMu   sync.Mutex
	globalSets = map[string]*RuleSet{}
)

// OpenGlobal returns the one shared global rule set for a state dir, so a
// rule added while one project is open is in force in every other.
func OpenGlobal(stateDir string) *RuleSet {
	globalMu.Lock()
	defer globalMu.Unlock()
	if rs, ok := globalSets[stateDir]; ok {
		return rs
	}
	rs := newRuleSet(ScopeGlobal, NewGlobalStore(stateDir), "")
	globalSets[stateDir] = rs
	return rs
}

func (rs *RuleSet) snapshot() []Rule {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make([]Rule, len(rs.rules))
	for i, r := range rs.rules {
		r.Scope = rs.scope
		out[i] = r
	}
	return out
}

func (rs *RuleSet) add(r Rule) (Rule, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.loadErr != nil {
		return Rule{}, fmt.Errorf("%w: %v", ErrRulesCorrupt, rs.loadErr)
	}
	for _, x := range rs.rules {
		if x.Tool == r.Tool && x.Decision == r.Decision && equalStrings(x.Pattern, r.Pattern) && equalStrings(x.Modes, r.Modes) {
			x.Scope = rs.scope
			return x, nil
		}
	}
	rs.rules = append(rs.rules, r)
	if rs.store != nil {
		if err := rs.store.Save(rs.rules); err != nil {
			rs.rules = rs.rules[:len(rs.rules)-1]
			return Rule{}, err
		}
	}
	r.Scope = rs.scope
	return r, nil
}

func (rs *RuleSet) remove(id string) (Rule, bool, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i, r := range rs.rules {
		if r.ID != id {
			continue
		}
		if rs.loadErr != nil {
			return Rule{}, false, fmt.Errorf("%w: %v", ErrRulesCorrupt, rs.loadErr)
		}
		kept := append(append([]Rule(nil), rs.rules[:i]...), rs.rules[i+1:]...)
		if rs.store != nil {
			if err := rs.store.Save(kept); err != nil {
				return Rule{}, false, err
			}
		}
		rs.rules = kept
		r.Scope = rs.scope
		return r, true, nil
	}
	return Rule{}, false, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Engine decides whether a tool call runs, asks, or is refused, from the
// rules in the session, the project and the global set.
//
// Precedence, across all scopes: any matching deny wins; then any matching
// ask; then any matching allow; otherwise the default (ask). Scope never
// changes that — a global deny beats a session allow.
type Engine struct {
	session    *RuleSet
	project    *RuleSet
	global     *RuleSet
	normalizer *normalizer.Normalizer
	presets    atomic.Bool
}

func New(norm *normalizer.Normalizer, store *PersistentStore) *Engine {
	e := &Engine{
		session:    &RuleSet{scope: ScopeSession},
		project:    &RuleSet{scope: ScopeProject},
		normalizer: norm,
	}
	if store != nil {
		e.project = newRuleSet(ScopeProject, store, norm.ProjectRoot)
	}
	return e
}

// SetGlobal attaches the shared global rule set (OpenGlobal).
func (e *Engine) SetGlobal(g *RuleSet) { e.global = g }

// SetPresets turns on the built-in allow rules for read-only commands (ls,
// git status/diff/log/show). Only meant for a project whose bash runs in a
// real bubblewrap sandbox.
func (e *Engine) SetPresets(on bool) { e.presets.Store(on) }

// Request is one tool call to decide. Mode is the session's mode ("" if the
// caller has none): it decides which allow rules are in force.
type Request struct {
	Tool   string
	Target string
	Mode   string
}

// Result is the engine's verdict. Rule is what decided it, nil for a
// built-in default. Suggestions are the rules a user may pick to stop being
// asked about calls like this one; empty unless the decision is Ask by default.
type Result struct {
	Decision    Decision
	Rule        *Rule
	CacheKey    string // legacy "tool:target", set when an exact suggestion exists
	Suggestions []Suggestion
	// Resolved is the path a file call's target resolved to (ResolveAndValidatePath)
	// when it was decided. The tool must still resolve to it when it runs.
	Resolved string
}

func (e *Engine) sets() []*RuleSet {
	s := []*RuleSet{e.session, e.project}
	if e.global != nil {
		s = append([]*RuleSet{e.global}, s...)
	}
	return s
}

func (e *Engine) allRules() (rules []Rule, corrupt bool) {
	for _, rs := range e.sets() {
		rs.mu.RLock()
		corrupt = corrupt || rs.loadErr != nil
		rs.mu.RUnlock()
		rules = append(rules, rs.snapshot()...)
	}
	if e.presets.Load() {
		rules = append(rules, presetRules()...)
	}
	return rules, corrupt
}

// ReadTool reports whether a tool only reads project files. Its calls are
// checked against read rules and the secret set, not asked about by default.
func ReadTool(name string) bool { return name == "file_read" || name == "search" }

func ruleCoversTool(r *Rule, tool string) bool {
	switch r.Tool {
	case ToolBash:
		return tool == "bash" || tool == "terminal_start" || tool == "terminal_output"
	case ToolEdit:
		return tool == "file_write" || tool == "file_edit"
	case ToolRead:
		return ReadTool(tool)
	default:
		return coversMCP(r.Tool, tool)
	}
}

// Check decides one tool call.
func (e *Engine) Check(req Request) (res Result) {
	rules, corrupt := e.allRules()
	subj := e.newSubject(req)
	if subj.resolveErr == nil {
		defer func() { res.Resolved = subj.resolved }()
	}

	var ask, allow *Rule
	for i := range rules {
		r := &rules[i]
		if !ruleCoversTool(r, req.Tool) || !r.appliesInMode(req.Mode) {
			continue
		}
		if !subj.matches(e, r) {
			continue
		}
		switch r.Decision {
		case DecisionDeny:
			return Result{Decision: DecisionDeny, Rule: r}
		case DecisionAsk:
			if ask == nil {
				ask = r
			}
		case DecisionAllow:
			if allow == nil {
				allow = r
			}
		}
	}

	if ReadTool(req.Tool) {
		if ask != nil {
			return Result{Decision: DecisionAsk, Rule: ask}
		}
		if subj.secret {
			return Result{Decision: DecisionAsk}
		}
		return Result{Decision: DecisionAllow}
	}
	if req.Tool == "bash" && subj.parseErr != nil {
		return Result{Decision: DecisionDeny}
	}
	if ask != nil {
		return Result{Decision: DecisionAsk, Rule: ask}
	}
	if allow != nil && !corrupt {
		return Result{Decision: DecisionAllow, Rule: allow}
	}
	res = Result{Decision: DecisionAsk, Suggestions: e.suggest(req, subj)}
	for _, s := range res.Suggestions {
		if s.ID == SuggestExact {
			res.CacheKey = req.Tool + ":" + req.Target
		}
	}
	return res
}

// subject is a request prepared for matching: the argv a command parses to,
// the path a file call resolves to.
type subject struct {
	req      Request
	sc       *normalizer.ShellCommand
	parseErr error
	cands    [][]string

	reqRel      string // path as requested, project-relative ("" if outside)
	resolvedRel string
	resolved    string
	resolveErr  error
	secret      bool // a read of a secret-shaped path (see secretPath)
}

func (e *Engine) newSubject(req Request) *subject {
	s := &subject{req: req}
	switch req.Tool {
	case "bash":
		s.sc, s.parseErr = parseArgv(req.Target)
		s.cands = commandCandidates(req.Target)
	case "terminal_start", "terminal_output":
		s.cands = commandCandidates(req.Target)
	case "file_write", "file_edit", "file_read", "search":
		s.reqRel = relSlash(e.normalizer.ProjectRoot, req.Target)
		s.resolved, s.resolveErr = e.normalizer.ResolveAndValidatePath(req.Target)
		if s.resolveErr == nil {
			s.resolvedRel = relSlash(e.normalizer.ProjectRoot, s.resolved)
		}
		// A symlink to a secret counts too: the resolved path is what gets read.
		s.secret = secretPath(req.Target) || (s.resolveErr == nil && secretPath(s.resolved))
	}
	return s
}

func (s *subject) matches(e *Engine, r *Rule) bool {
	allow := r.Decision == DecisionAllow
	switch r.Tool {
	case ToolBash:
		if allow {
			// A terminal is a real shell and typed input is keystrokes:
			// only the bash tool's plain argv can be allowed by a rule.
			return s.req.Tool == "bash" && s.sc != nil && allowBashMatches(r, s.sc)
		}
		for _, c := range s.cands {
			if matchArgvPattern(r.Pattern, c, true) {
				return true
			}
		}
		return false
	case ToolEdit, ToolFileWrite, ToolFileEdit, ToolRead:
		pat := r.Pattern[0]
		if allow {
			return s.resolveErr == nil && s.reqRel != "" && !neverAllow(e.normalizer, s.resolved) &&
				matchPathPattern(pat, s.reqRel, false) && matchPathPattern(pat, s.resolvedRel, false)
		}
		return (s.reqRel != "" && matchPathPattern(pat, s.reqRel, true)) ||
			(s.resolvedRel != "" && matchPathPattern(pat, s.resolvedRel, true))
	default:
		if _, _, ok := ParseMCPTool(r.Tool); ok || r.Tool == ToolWebSearch {
			return true // covering the tool was the whole question
		}
		return len(r.Pattern) == 1 && strings.EqualFold(r.Pattern[0], s.req.Target)
	}
}

// --- suggestions ---

// Suggestion IDs for the two rules most requests offer.
const (
	SuggestExact  = "exact"  // this very command / file / host
	SuggestPrefix = "prefix" // `go test *`
	SuggestDir    = "dir"    // edits under a directory
	SuggestServer = "server" // every tool of one MCP server
)

// Suggestion is a rule a user may pick on a permission_request. The server
// keeps the suggestions it offered and rebuilds the rule from its own copy
// when the answer names one, so a client never supplies a pattern.
type Suggestion struct {
	ID      string
	Label   string
	Tool    string
	Pattern []string
	Scope   Scope // the scope offered by default
}

func (e *Engine) suggest(req Request, subj *subject) []Suggestion {
	switch req.Tool {
	case "bash":
		if subj.parseErr != nil || subj.sc.HasChain || len(subj.sc.Argv) == 0 || e.normalizer.IsDestructiveBash(subj.sc) {
			return nil
		}
		argv := subj.sc.Argv
		var out []Suggestion
		if n := prefixLen(argv); n > 0 {
			pat := append(append([]string(nil), argv[:n]...), "*")
			if unsafeWildcard(argv[:n]) == "" {
				out = append(out, Suggestion{ID: SuggestPrefix, Tool: ToolBash, Pattern: pat, Scope: ScopeProject,
					Label: "Always allow `" + (&Rule{Tool: ToolBash, Pattern: pat}).Text() + "`"})
			}
		}
		exact := append([]string(nil), argv...)
		if !slices.Contains(exact, "*") {
			out = append(out, Suggestion{ID: SuggestExact, Tool: ToolBash, Pattern: exact, Scope: ScopeProject,
				Label: "Always allow this exact command"})
		}
		return out
	case "file_write", "file_edit":
		if subj.resolveErr != nil || subj.reqRel == "" || neverAllow(e.normalizer, subj.resolved) {
			return nil
		}
		rel := subj.resolvedRel
		out := []Suggestion{}
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "" {
			out = append(out, Suggestion{ID: SuggestDir, Tool: ToolEdit, Pattern: []string{escapeGlob(dir) + "/**"}, Scope: ScopeProject,
				Label: "Allow edits under `" + dir + "/`"})
		}
		return append(out, Suggestion{ID: SuggestExact, Tool: ToolEdit, Pattern: []string{escapeGlob(rel)}, Scope: ScopeProject,
			Label: "Allow edits to `" + rel + "`"})
	case ToolWebSearch:
		return []Suggestion{{ID: SuggestExact, Tool: ToolWebSearch, Pattern: []string{"*"}, Scope: ScopeProject, Label: "Always allow web searches"}}
	case "web_fetch", "browser":
		if req.Target == "" {
			return nil
		}
		what := "Always allow `" + req.Target + "`"
		if req.Tool == "web_fetch" {
			what = "Always allow fetching from `" + req.Target + "`"
		}
		return []Suggestion{{ID: SuggestExact, Tool: req.Tool, Pattern: []string{req.Target}, Scope: ScopeProject, Label: what}}
	}
	if server, name, ok := ParseMCPTool(req.Tool); ok && name != "*" {
		return []Suggestion{
			{ID: SuggestExact, Tool: req.Tool, Pattern: []string{"*"}, Scope: ScopeProject, Label: "Always allow `" + name + "` from " + server},
			{ID: SuggestServer, Tool: ToolMCPPrefix + server + "__*", Pattern: []string{"*"}, Scope: ScopeProject, Label: "Always allow every tool from " + server},
		}
	}
	return nil // terminal_start / terminal_output: a real shell, never rule-allowed
}

// prefixLen is how many leading argv tokens a wildcard suggestion pins: the
// program, plus its subcommand for tools that have one, plus the script name
// for `npm run <script>`. 0 when there is no safe prefix.
func prefixLen(argv []string) int {
	base := filepath.Base(argv[0])
	if argv[0] != base || strings.Contains(argv[0], "=") || strings.HasPrefix(argv[0], "-") {
		return 0
	}
	if !subcommandTools[base] {
		return 1
	}
	if len(argv) < 2 || strings.HasPrefix(argv[1], "-") {
		return 0
	}
	if pkgRunners[base] && argv[1] == "run" {
		if len(argv) < 3 || strings.HasPrefix(argv[2], "-") {
			return 0
		}
		return 3
	}
	return 2
}

// --- recording and managing rules ---

// Remember turns a user's answer into a stored rule. It is the single place
// a permission decision becomes a rule: the agent calls it for a picked
// suggestion, and an audit trail belongs here. mode is the session's mode
// when it was asked (an allow only applies from that mode up). A denial
// stores a deny rule with the suggestion's pattern.
func (e *Engine) Remember(sug Suggestion, allowed bool, scope Scope, mode string) (Rule, error) {
	if scope == "" {
		scope = sug.Scope
	}
	r := Rule{Tool: sug.Tool, Pattern: append([]string(nil), sug.Pattern...), Decision: DecisionDeny, Source: SourceSuggested}
	if allowed {
		r.Decision = DecisionAllow
		r.Modes = RatchetModes(mode)
	}
	return e.AddRule(scope, r)
}

// AddRule validates and stores a rule in a scope. Presets are not a scope.
func (e *Engine) AddRule(scope Scope, r Rule) (Rule, error) {
	var rs *RuleSet
	switch scope {
	case ScopeSession:
		rs = e.session
	case ScopeProject, "":
		scope, rs = ScopeProject, e.project
	case ScopeGlobal:
		rs = e.global
	}
	if rs == nil {
		return Rule{}, fmt.Errorf("no %s rule store", scope)
	}
	if r.ID == "" {
		r.ID = newRuleID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Source == "" {
		r.Source = SourceUser
	}
	r.Scope = ""
	if err := r.Validate(scope); err != nil {
		return Rule{}, err
	}
	return rs.add(r)
}

// RemoveRule deletes a rule by id from whichever scope holds it.
func (e *Engine) RemoveRule(id string) (Rule, error) {
	if strings.HasPrefix(id, "preset:") {
		return Rule{}, fmt.Errorf("built-in rules cannot be removed; add an ask rule to override one")
	}
	for _, rs := range e.sets() {
		if r, ok, err := rs.remove(id); err != nil {
			return Rule{}, err
		} else if ok {
			return r, nil
		}
	}
	return Rule{}, fmt.Errorf("no rule %s", id)
}

// Rules lists every rule in force (global, project, session, then presets).
func (e *Engine) Rules() []Rule {
	r, _ := e.allRules()
	return r
}

// Problems reports rule files that failed to load, and v1 entries dropped by
// migration, for the rules UI.
func (e *Engine) Problems() (errs, notes []string) {
	for _, rs := range e.sets() {
		rs.mu.RLock()
		if rs.loadErr != nil {
			errs = append(errs, fmt.Sprintf("%s rules: %v. Every approval asks until it is fixed, and rules cannot be changed", rs.scope, rs.loadErr))
		}
		notes = append(notes, rs.notes...)
		rs.mu.RUnlock()
	}
	return errs, notes
}
