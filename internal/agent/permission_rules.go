package agent

import (
	"fmt"
	"log"

	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
)

// PermissionChoice is the rule-related part of a permission_response: which
// of the request's suggestions to remember, and in what scope. The zero value
// is a plain answer (or, with remember, the legacy exact-match rule).
type PermissionChoice struct {
	RuleID string
	Scope  string
	// By names the device that answered, for the audit log.
	By string
}

// permAsk is what the agent keeps about an open permission_request from its
// own loop, to turn the answer into a rule.
type permAsk struct {
	mode        Mode
	suggestions []permission.Suggestion
	// tool and target say what is being asked about, for the audit log.
	tool, target string
	session      string
}

func wireSuggestions(ss []permission.Suggestion) []protocol.PermissionSuggestion {
	out := make([]protocol.PermissionSuggestion, len(ss))
	for i, s := range ss {
		out[i] = protocol.PermissionSuggestion{ID: s.ID, Label: s.Label, Tool: s.Tool, Pattern: s.Pattern, Scope: string(s.Scope)}
	}
	return out
}

// rememberPermission is the one place an answer to a permission_request
// becomes a stored rule (see permission.Engine.Remember). A picked
// suggestion is remembered as picked; a bare remember:true means the exact
// suggestion (this command / file / host), as it always did. Denying with
// remember stores a deny rule. Failures only mean the rule isn't stored —
// the call itself is still allowed or refused as answered.
func (a *Agent) rememberPermission(p permAsk, allowed, remember bool, ch PermissionChoice) *permission.Rule {
	id := ch.RuleID
	if id == "" && remember {
		id = permission.SuggestExact
	}
	if id == "" {
		return nil
	}
	var sug *permission.Suggestion
	for i := range p.suggestions {
		if p.suggestions[i].ID == id {
			sug = &p.suggestions[i]
		}
	}
	if sug == nil {
		return nil
	}
	scope := permission.Scope(ch.Scope)
	switch scope {
	case "", permission.ScopeSession, permission.ScopeProject, permission.ScopeGlobal:
	default:
		scope = ""
	}
	rule, err := a.permEngine.Remember(*sug, allowed, scope, string(p.mode))
	if err != nil {
		log.Printf("permissions: could not remember %q: %v", sug.Label, err)
		return nil
	}
	if scope == "" {
		scope = sug.Scope
	}
	a.auditRule(ch.By, rule, scope)
	return &rule
}

// denyReason words a rule's refusal for the model.
func denyReason(res permission.Result) string {
	if res.Rule != nil {
		return fmt.Sprintf("permission denied by a %s rule (%s %s)", res.Rule.Scope, res.Rule.Tool, res.Rule.Text())
	}
	return "permission denied by policy: the command could not be parsed"
}
