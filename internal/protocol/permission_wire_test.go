package protocol

import (
	"encoding/json"
	"testing"
)

// An old client's permission_response (no ruleId/scope) still decodes, and a
// new one carries them.
func TestPermissionResponseWireCompat(t *testing.T) {
	var old PermissionResponseData
	if err := json.Unmarshal([]byte(`{"allowed":true,"remember":true,"feedback":""}`), &old); err != nil {
		t.Fatal(err)
	}
	if !old.Allowed || !old.Remember || old.RuleID != "" || old.Scope != "" {
		t.Fatalf("%+v", old)
	}
	var picked PermissionResponseData
	if err := json.Unmarshal([]byte(`{"allowed":true,"remember":false,"ruleId":"prefix","scope":"session"}`), &picked); err != nil {
		t.Fatal(err)
	}
	if picked.RuleID != "prefix" || picked.Scope != "session" {
		t.Fatalf("%+v", picked)
	}
	b, _ := json.Marshal(PermissionResponseData{Allowed: true})
	if string(b) != `{"allowed":true,"remember":false}` {
		t.Errorf("an answer with no rule grew fields: %s", b)
	}
}

// A request with no suggestions serialises exactly as before; with them, it lists them.
func TestPermissionRequestWireSuggestions(t *testing.T) {
	b, _ := json.Marshal(PermissionRequestData{ToolName: "bash", RequiresAsk: true})
	if string(b) != `{"toolName":"bash","description":"","requiresAsk":true,"cacheKey":""}` {
		t.Errorf("legacy shape changed: %s", b)
	}
	b, _ = json.Marshal(PermissionRequestData{ToolName: "bash", Suggestions: []PermissionSuggestion{{ID: "prefix", Label: "Always allow `go test *`", Tool: "bash", Pattern: []string{"go", "test", "*"}, Scope: "project"}, {ID: "agent", Label: "x", Native: true}}})
	var back PermissionRequestData
	if err := json.Unmarshal(b, &back); err != nil || len(back.Suggestions) != 2 || back.Suggestions[0].Pattern[2] != "*" || !back.Suggestions[1].Native {
		t.Fatalf("%v %+v", err, back)
	}
}
