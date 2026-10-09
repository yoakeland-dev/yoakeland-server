package agent

import (
	"strings"

	"yoakeland/internal/audit"
	"yoakeland/internal/permission"
)

// audit reports e to the server's audit log (Hooks.Audit), stamped with this
// session. Never called with a.mu held.
func (a *Agent) audit(e audit.Event) {
	if a.hooks.Audit == nil {
		return
	}
	if e.Session == "" {
		e.Session = a.CurrentSessionID()
	}
	if e.Actor == "" {
		e.Actor = "server"
	}
	a.hooks.Audit(e)
}

// auditAuto records a permission answered without asking a person: by a rule,
// by the mode (Accept Edits), by autonomous mode's bypass, or by policy.
func (a *Agent) auditAuto(session, tool, target, outcome string, mode Mode, rule *permission.Rule, via string) {
	d := map[string]any{"mode": string(mode), "via": via}
	if rule != nil {
		d["ruleId"], d["rule"], d["ruleScope"] = rule.ID, rule.Text(), string(rule.Scope)
	}
	a.audit(audit.Event{Kind: audit.PermAuto, Session: session, Tool: tool, Target: target, Outcome: outcome, Detail: d})
}

// viaRule words how an allowed call got through, for tool_exec.
func viaRule(rule *permission.Rule) string {
	if rule == nil {
		return "policy"
	}
	return "rule:" + rule.ID
}

// isPreset: a built-in read-only allow, too routine to log as its own event.
func isPreset(rule *permission.Rule) bool {
	return rule != nil && strings.HasPrefix(rule.ID, "preset:")
}

// auditExec records a tool that can change things (or reach out) being run.
func (a *Agent) auditExec(session, tool, target string, mode Mode, via string) {
	a.audit(audit.Event{Kind: audit.ToolExec, Session: session, Tool: tool, Target: target, Outcome: "run", Detail: map[string]any{"mode": string(mode), "via": via}})
}

// auditRule records a permission rule stored from an answer.
func (a *Agent) auditRule(by string, r permission.Rule, scope permission.Scope) {
	a.audit(audit.Event{Kind: audit.RuleAdd, Actor: by, Tool: r.Tool, Target: r.Text(), Outcome: string(r.Decision), Detail: map[string]any{
		"ruleId": r.ID, "scope": string(scope), "source": r.Source, "modes": r.Modes,
	}})
}

func outcomeOf(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}
