package permission_test

import (
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
)

func TestPermissionEngine_EvaluationAndCaching(t *testing.T) {
	norm, _ := normalizer.New(t.TempDir())
	engine := permission.New(norm)

	// file_read always allowed
	dec, _ := engine.Evaluate("file_read", "config.json")
	if dec != permission.DecisionAllow {
		t.Fatalf("expected allow for file_read, got %s", dec)
	}

	// safe bash command
	dec, _ = engine.Evaluate("bash", "pwd")
	if dec != permission.DecisionAllow {
		t.Fatalf("expected allow for safe bash pwd, got %s", dec)
	}

	// destructive bash command
	dec, key := engine.Evaluate("bash", "rm -rf /")
	if dec != permission.DecisionAsk {
		t.Fatalf("expected ask for destructive rm -rf, got %s", dec)
	}

	// Record explicit deny
	engine.RecordDecision(key, false)
	dec, _ = engine.Evaluate("bash", "rm -rf /")
	if dec != permission.DecisionDeny {
		t.Fatalf("expected deny from session cache, got %s", dec)
	}

	// Record explicit allow
	engine.RecordDecision(key, true)
	dec, _ = engine.Evaluate("bash", "rm -rf /")
	if dec != permission.DecisionAllow {
		t.Fatalf("expected allow from session cache, got %s", dec)
	}
}
