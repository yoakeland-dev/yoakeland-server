package permission_test

import (
	"testing"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
)

func TestPermissionEngine_DiskPersistence(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	store := permission.NewPersistentStore(tempDir)

	engine1 := permission.New(norm, store)

	// Baseline evaluate: file_write requires ask
	dec, key := engine1.Evaluate("file_write", "main.go")
	if dec != permission.DecisionAsk {
		t.Fatalf("expected ask, got %s", dec)
	}

	// Record allowed with persistence = true
	engine1.RecordDecision(key, true, true)

	// Check engine 1 allows it
	dec, _ = engine1.Evaluate("file_write", "main.go")
	if dec != permission.DecisionAllow {
		t.Fatalf("expected allow after record, got %s", dec)
	}

	// Spin up a brand new Engine instance representing process restart
	engine2 := permission.New(norm, store)
	dec, _ = engine2.Evaluate("file_write", "main.go")
	if dec != permission.DecisionAllow {
		t.Fatalf("expected allow persisted across restarts, got %s", dec)
	}
}

// TestPermissionEngine_BashAlwaysAsks: no command is auto-allowed, not even
// "read-only" ones — git diff --output= writes a file, --no-index reads
// outside the project, --ext-diff runs a configured program.
func TestPermissionEngine_BashAlwaysAsks(t *testing.T) {
	norm, _ := normalizer.New(t.TempDir())
	engine := permission.New(norm, nil)
	for _, cmd := range []string{"ls", "pwd", "echo hi", "whoami", "git status", "git diff --output=x", "git diff --no-index /etc/passwd /dev/null"} {
		if dec, _ := engine.Evaluate("bash", cmd); dec != permission.DecisionAsk {
			t.Errorf("Evaluate(bash, %q) = %s, want ask", cmd, dec)
		}
	}
}
