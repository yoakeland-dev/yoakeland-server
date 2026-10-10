package permission_test

import (
	"testing"

	"yoakeland/internal/permission"
)

func mcpRule(d permission.Decision, tool string) permission.Rule {
	return permission.Rule{Tool: tool, Pattern: []string{"*"}, Decision: d}
}

func TestMCPToolsAskByDefaultAndSuggestTwoRules(t *testing.T) {
	e, _ := newEngine(t)
	res := e.Check(permission.Request{Tool: "mcp__github__create_issue", Target: `{"title":"x"}`, Mode: "read_write"})
	if res.Decision != permission.DecisionAsk {
		t.Fatalf("decision = %s, want ask", res.Decision)
	}
	var ids []string
	for _, s := range res.Suggestions {
		ids = append(ids, s.ID+"="+s.Tool)
	}
	if len(ids) != 2 || ids[0] != "exact=mcp__github__create_issue" || ids[1] != "server=mcp__github__*" {
		t.Fatalf("suggestions = %v", ids)
	}
}

func TestMCPRuleCoversTheToolOrTheServer(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, mcpRule(permission.DecisionAllow, "mcp__github__create_issue"))
	mustAdd(t, e, permission.ScopeProject, mcpRule(permission.DecisionAllow, "mcp__docs__*"))
	mustAdd(t, e, permission.ScopeProject, mcpRule(permission.DecisionDeny, "mcp__docs__delete_page"))
	for _, c := range []struct {
		tool string
		want permission.Decision
	}{
		{"mcp__github__create_issue", permission.DecisionAllow},
		{"mcp__github__delete_repo", permission.DecisionAsk}, // another tool of the same server
		{"mcp__docs__search", permission.DecisionAllow},      // the whole server
		{"mcp__docs__delete_page", permission.DecisionDeny},  // a deny beats the server-wide allow
		{"mcp__github2__create_issue", permission.DecisionAsk},
		{"mcp__docsx__search", permission.DecisionAsk}, // a prefix of a name is not the server
	} {
		if got := check(e, c.tool, "{}", "read_write"); got != c.want {
			t.Errorf("%s = %s, want %s", c.tool, got, c.want)
		}
	}
}

func TestMCPServerRuleIgnoresServersWhoseNameExtendsIt(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, mcpRule(permission.DecisionAllow, "mcp__foo__*"))
	for _, c := range []struct {
		tool string
		want permission.Decision
	}{
		{"mcp__foo__search", permission.DecisionAllow},
		{"mcp__foo___search", permission.DecisionAsk}, // server "foo_", tool "search"
	} {
		if got := check(e, c.tool, "{}", "read_write"); got != c.want {
			t.Errorf("%s = %s, want %s", c.tool, got, c.want)
		}
	}
}

func TestMCPAllowNeverWidensReadOnlyOrPlan(t *testing.T) {
	e, _ := newEngine(t)
	mustAdd(t, e, permission.ScopeProject, mcpRule(permission.DecisionAllow, "mcp__docs__*"))
	for _, mode := range []string{"read_only", "plan"} {
		if got := check(e, "mcp__docs__search", "{}", mode); got == permission.DecisionAllow {
			t.Errorf("an allow rule applied in %s", mode)
		}
	}
}

func TestMCPRulesAreValidated(t *testing.T) {
	e, _ := newEngine(t)
	for _, bad := range []permission.Rule{
		{Tool: "mcp__", Pattern: []string{"*"}, Decision: permission.DecisionAllow},
		{Tool: "mcp__server", Pattern: []string{"*"}, Decision: permission.DecisionAllow}, // no tool part
		{Tool: "mcp____tool", Pattern: []string{"*"}, Decision: permission.DecisionAllow},
		{Tool: "mcp__bad name__tool", Pattern: []string{"*"}, Decision: permission.DecisionAllow},
		{Tool: "mcp__s__t", Pattern: []string{"anything"}, Decision: permission.DecisionAllow}, // a rule can't be about arguments
		{Tool: "mcp__s__t", Pattern: nil, Decision: permission.DecisionAllow},
		{Tool: "other_tool", Pattern: []string{"*"}, Decision: permission.DecisionAllow},
	} {
		if _, err := e.AddRule(permission.ScopeProject, bad); err == nil {
			t.Errorf("rule %+v should have been refused", bad)
		}
	}
	if _, err := e.AddRule(permission.ScopeGlobal, mcpRule(permission.DecisionDeny, "mcp__s__*")); err == nil {
		t.Log("(no global store in this test engine; fine)")
	}
}

func TestMCPRememberStoresARuleThatThenApplies(t *testing.T) {
	e, _ := newEngine(t)
	res := e.Check(permission.Request{Tool: "mcp__github__create_issue", Mode: "accept_edits"})
	var server permission.Suggestion
	for _, s := range res.Suggestions {
		if s.ID == permission.SuggestServer {
			server = s
		}
	}
	rule, err := e.Remember(server, true, permission.ScopeProject, "accept_edits")
	if err != nil {
		t.Fatal(err)
	}
	if rule.Text() != "mcp__github__*" {
		t.Fatalf("text = %q", rule.Text())
	}
	// Allowed in the mode it was granted in and above, not below (the ratchet).
	if got := check(e, "mcp__github__list_repos", "{}", "auto"); got != permission.DecisionAllow {
		t.Errorf("in auto = %s", got)
	}
	if got := check(e, "mcp__github__list_repos", "{}", "read_write"); got != permission.DecisionAsk {
		t.Errorf("in read_write = %s", got)
	}
}
