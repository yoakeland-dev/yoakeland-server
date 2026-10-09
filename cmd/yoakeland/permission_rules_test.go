package main

import (
	"encoding/json"
	"strings"
	"testing"

	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
)

func rulesReply(t *testing.T, envs []protocol.Envelope, i int) protocol.PermissionRulesData {
	t.Helper()
	if i >= len(envs) || envs[i].Type != protocol.EventPermissionRules {
		t.Fatalf("reply %d is not permission_rules: %+v", i, envs)
	}
	var d protocol.PermissionRulesData
	if err := json.Unmarshal(envs[i].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestServeConnection_PermissionRulesAddListDelete(t *testing.T) {
	deps := newTestDeps(t)
	add := func(scope string, r protocol.PermissionRuleData) string {
		return envelopeJSON(t, protocol.EventAddPermissionRule, protocol.AddPermissionRuleData{Scope: scope, Rule: r})
	}
	envs := runDispatch(t, deps,
		add("project", protocol.PermissionRuleData{Tool: "bash", Text: "go test *", Decision: "allow"}),
		add("global", protocol.PermissionRuleData{Tool: "bash", Text: "terraform destroy *", Decision: "deny"}),
		add("project", protocol.PermissionRuleData{Tool: "bash", Text: "sh -c *", Decision: "allow"}),                                      // too broad
		add("project", protocol.PermissionRuleData{Tool: "edit", Text: "**", Decision: "allow"}),                                           // the whole project
		add("global", protocol.PermissionRuleData{Tool: "edit", Text: "docs/**", Decision: "allow"}),                                       // a path is not global
		add("project", protocol.PermissionRuleData{Tool: "bash", Text: "go vet *", Decision: "allow", NotMatch: []string{"go vet ./..."}}), // fails its example
		envelopeJSON(t, protocol.EventListPermissionRules, protocol.ListPermissionRulesData{}),
	)
	if len(envs) != 7 {
		t.Fatalf("%d replies", len(envs))
	}
	if r := rulesReply(t, envs, 0); r.Error != "" || len(r.Rules) < 1 {
		t.Fatalf("first add: %+v", r)
	}
	for i := 2; i <= 5; i++ {
		if r := rulesReply(t, envs, i); r.Error == "" {
			t.Errorf("reply %d: expected an error, got %+v", i, r)
		}
	}
	list := rulesReply(t, envs, 6)
	var goTest, tf *protocol.PermissionRuleData
	for i, r := range list.Rules {
		switch r.Text {
		case "go test *":
			goTest = &list.Rules[i]
		case "terraform destroy *":
			tf = &list.Rules[i]
		}
	}
	if goTest == nil || goTest.Scope != "project" || goTest.Decision != "allow" || strings.Join(goTest.Pattern, " ") != "go test *" || goTest.Source != "user" {
		t.Fatalf("go test rule = %+v", goTest)
	}
	if tf == nil || tf.Scope != "global" || tf.Decision != "deny" {
		t.Fatalf("terraform rule = %+v", tf)
	}

	// The engine really uses them, and deleting one revokes it.
	envs = runDispatch(t, deps,
		envelopeJSON(t, protocol.EventDeletePermissionRule, protocol.DeletePermissionRuleData{ID: goTest.ID}),
		envelopeJSON(t, protocol.EventDeletePermissionRule, protocol.DeletePermissionRuleData{ID: "r_nope"}),
		envelopeJSON(t, protocol.EventDeletePermissionRule, protocol.DeletePermissionRuleData{ID: "preset:ls"}),
	)
	if r := rulesReply(t, envs, 0); r.Error != "" {
		t.Fatalf("delete: %+v", r)
	}
	for _, r := range rulesReply(t, envs, 0).Rules {
		if r.ID == goTest.ID {
			t.Error("deleted rule still listed")
		}
	}
	if rulesReply(t, envs, 1).Error == "" || rulesReply(t, envs, 2).Error == "" {
		t.Error("deleting an unknown rule or a preset should fail")
	}
}

func TestPermissionRuleWireRoundTrip(t *testing.T) {
	r := permission.Rule{ID: "r1", Tool: "bash", Pattern: []string{"echo", "two words", "*"}, Decision: permission.DecisionAllow, Scope: permission.ScopeProject}
	w := toWireRule(r)
	if w.Text != "echo 'two words' *" {
		t.Fatalf("text = %q", w.Text)
	}
	back := fromWireRule(protocol.PermissionRuleData{Tool: "bash", Text: w.Text, Decision: "allow"})
	if strings.Join(back.Pattern, "|") != "echo|two words|*" {
		t.Fatalf("pattern = %q", back.Pattern)
	}
}
