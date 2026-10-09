package main

import (
	"encoding/json"
	"strings"
	"time"

	"yoakeland/internal/audit"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
)

// Permission rules over the wire (SPEC.md §3.1 "Permission rules"): list,
// add and delete, all answered with the project's full rule list. A change is
// also broadcast to every peer so a second device sees it at once.

func toWireRule(r permission.Rule) protocol.PermissionRuleData {
	w := protocol.PermissionRuleData{
		ID: r.ID, Tool: r.Tool, Pattern: r.Pattern, Text: r.Text(), Decision: string(r.Decision),
		Scope: string(r.Scope), Modes: r.Modes, Source: r.Source, Match: r.Match, NotMatch: r.NotMatch,
		Readonly: r.Scope == permission.ScopeBuiltin,
	}
	if !r.CreatedAt.IsZero() {
		w.CreatedAt = r.CreatedAt.Format(time.RFC3339)
	}
	return w
}

// fromWireRule builds a rule from what a client typed. Text wins over
// Pattern when both are given: bash text is split like a command line, so
// `go test *` becomes ["go","test","*"]; other tools take the text as is.
func fromWireRule(w protocol.PermissionRuleData) permission.Rule {
	r := permission.Rule{
		Tool: w.Tool, Pattern: w.Pattern, Decision: permission.Decision(w.Decision), Modes: w.Modes,
		Match: w.Match, NotMatch: w.NotMatch, Source: permission.SourceUser,
	}
	if text := strings.TrimSpace(w.Text); text != "" {
		if w.Tool == permission.ToolBash {
			if sc, err := (&normalizer.Normalizer{}).ParseShellArgv(text); err == nil {
				r.Pattern = sc.Argv
			} else {
				r.Pattern = nil
			}
		} else {
			r.Pattern = []string{text}
		}
	}
	return r
}

func permissionRulesFor(pc *ProjectContext) protocol.PermissionRulesData {
	out := protocol.PermissionRulesData{ProjectPath: pc.Root, Rules: []protocol.PermissionRuleData{}}
	for _, r := range pc.Permissions.Rules() {
		out.Rules = append(out.Rules, toWireRule(r))
	}
	out.Errors, out.Notes = pc.Permissions.Problems()
	return out
}

func handlePermissionRuleRequest(ipc *protocol.IPCStream, deps connDeps, peerID string, env *protocol.Envelope) {
	var path string
	var add *protocol.AddPermissionRuleData
	var del *protocol.DeletePermissionRuleData
	switch env.Type {
	case protocol.EventListPermissionRules:
		var d protocol.ListPermissionRulesData
		_ = json.Unmarshal(env.Data, &d)
		path = d.ProjectPath
	case protocol.EventAddPermissionRule:
		add = &protocol.AddPermissionRuleData{}
		_ = json.Unmarshal(env.Data, add)
		path = add.ProjectPath
	case protocol.EventDeletePermissionRule:
		del = &protocol.DeletePermissionRuleData{}
		_ = json.Unmarshal(env.Data, del)
		path = del.ProjectPath
	}
	if path == "" {
		path = deps.defaultProjectRoot
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventPermissionRules, env.SessionID, env.TurnID, "", protocol.PermissionRulesData{ProjectPath: path, Rules: []protocol.PermissionRuleData{}, Error: msg})
	}
	if path == "" {
		fail("no project")
		return
	}
	pc, err := deps.projects.Get(path)
	if err != nil {
		fail(err.Error())
		return
	}
	switch {
	case add != nil:
		rule, err := pc.Permissions.AddRule(permission.Scope(add.Scope), fromWireRule(add.Rule))
		if err != nil {
			fail(err.Error())
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.RuleAdd, Project: pc.Root, Tool: rule.Tool, Target: rule.Text(), Outcome: string(rule.Decision), Detail: map[string]any{
			"ruleId": rule.ID, "scope": add.Scope, "source": rule.Source, "modes": rule.Modes,
		}})
	case del != nil:
		rule, err := pc.Permissions.RemoveRule(del.ID)
		if err != nil {
			fail(err.Error())
			return
		}
		deps.record(ipc, peerID, audit.Event{Kind: audit.RuleRemove, Project: pc.Root, Tool: rule.Tool, Target: rule.Text(), Outcome: string(rule.Decision), Detail: map[string]any{
			"ruleId": rule.ID, "source": rule.Source,
		}})
	default:
		_ = ipc.SendEvent(protocol.EventPermissionRules, env.SessionID, env.TurnID, "", permissionRulesFor(pc))
		return
	}
	if deps.peers != nil && peerID != "" {
		deps.peers.BroadcastEvent("", protocol.EventPermissionRules, "", permissionRulesFor(pc))
	} else {
		_ = ipc.SendEvent(protocol.EventPermissionRules, env.SessionID, env.TurnID, "", permissionRulesFor(pc))
	}
}
