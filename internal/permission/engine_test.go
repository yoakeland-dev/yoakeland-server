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

// TestPermissionEngine_BashAutoAllowlist guards the static bash policy: a
// narrow, fixed set of commands auto-approve without a permission prompt,
// and — the case this test exists to pin down — ls only auto-approves when
// every path argument stays inside the project, since ls's arguments are
// attacker-meaningful (unlike pwd/whoami/echo's).
func TestPermissionEngine_BashAutoAllowlist(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)

	cases := []struct {
		name    string
		command string
		want    permission.Decision
	}{
		{"bare ls", "ls", permission.DecisionAllow},
		{"ls with dot", "ls .", permission.DecisionAllow},
		{"ls relative subdir", "ls src", permission.DecisionAllow},
		{"ls with flags only", "ls -la", permission.DecisionAllow},
		{"ls flags plus in-project path", "ls -la src", permission.DecisionAllow},
		{"ls absolute path outside project", "ls /etc", permission.DecisionAsk},
		{"ls traversal outside project", "ls ../../etc", permission.DecisionAsk},
		// No shell is ever invoked (bash.go execs argv directly), so "~" is
		// just a literal filename here, not shell tilde-expansion to
		// $HOME — it resolves to <project>/~ and correctly stays allowed.
		{"literal tilde argument (no shell expansion)", "ls ~", permission.DecisionAllow},
		{"bare pwd", "pwd", permission.DecisionAllow},
		{"bare whoami", "whoami", permission.DecisionAllow},
		{"echo with arbitrary text", "echo anything at all", permission.DecisionAllow},
		{"git status", "git status", permission.DecisionAllow},
		{"git diff", "git diff", permission.DecisionAllow},
		{"git reset is destructive, not allowlisted", "git reset --hard", permission.DecisionAsk},
		{"arbitrary command still asks", "curl http://example.com", permission.DecisionAsk},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, _ := engine.Evaluate("bash", tc.command)
			if dec != tc.want {
				t.Errorf("Evaluate(bash, %q) = %s, want %s", tc.command, dec, tc.want)
			}
		})
	}
}
