package agent_test

import (
	"testing"
	"yoakeland/internal/agent"
)

func TestUsageStore_AccumulatesAndPersists(t *testing.T) {
	tempDir := t.TempDir()

	store, err := agent.NewUsageStore(tempDir)
	if err != nil {
		t.Fatalf("NewUsageStore: %v", err)
	}

	sess, project := store.Record("session-A", 100, 50, 0.01)
	if sess.InputTokens != 100 || sess.OutputTokens != 50 || sess.CostUSD != 0.01 {
		t.Fatalf("unexpected first-record session totals: %+v", sess)
	}
	if project.InputTokens != 100 || project.CostUSD != 0.01 {
		t.Fatalf("unexpected first-record project totals: %+v", project)
	}

	sess, project = store.Record("session-A", 10, 5, 0.001)
	if sess.InputTokens != 110 || sess.CostUSD < 0.0109 || sess.CostUSD > 0.0111 {
		t.Fatalf("session totals didn't accumulate: %+v", sess)
	}

	// A second session's usage must not bleed into the first session's total,
	// but must still add into the project-wide total.
	_, project = store.Record("session-B", 1000, 0, 1.0)
	if project.InputTokens != 1110 {
		t.Fatalf("project total didn't accumulate across sessions: %+v", project)
	}

	// Reopening against the same project root must recover what was persisted.
	reopened, err := agent.NewUsageStore(tempDir)
	if err != nil {
		t.Fatalf("re-opening UsageStore: %v", err)
	}
	_, project = reopened.Record("session-A", 0, 0, 0)
	if project.InputTokens != 1110 {
		t.Fatalf("usage.json wasn't persisted/reloaded: project totals = %+v", project)
	}
}

func TestUsageStore_SessionTotal(t *testing.T) {
	store, err := agent.NewUsageStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewUsageStore: %v", err)
	}

	if total := store.SessionTotal("never-recorded"); total.CostUSD != 0 || total.InputTokens != 0 {
		t.Fatalf("expected a zero-value total for an unknown session, got %+v", total)
	}

	store.Record("session-A", 100, 50, 0.5)
	if total := store.SessionTotal("session-A"); total.CostUSD != 0.5 {
		t.Fatalf("SessionTotal = %+v, want CostUSD 0.5", total)
	}
	// SessionTotal must not itself record anything.
	if total := store.SessionTotal("session-A"); total.CostUSD != 0.5 {
		t.Fatalf("SessionTotal appears to have mutated state on read: %+v", total)
	}
}
