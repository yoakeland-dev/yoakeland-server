package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

func TestParseGitStatus(t *testing.T) {
	out := "# branch.oid abc\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +3 -1\n" +
		"1 .M N... 100644 100644 100644 a b cmd/x.go\n? new file.txt\n" +
		"1 A. N... 000000 100644 100644 0 b added.go\n" +
		"2 R. N... 100644 100644 100644 a b R100 new name.go\told.go\n" +
		"u UU N... 100644 100644 100644 100644 a b c conflict.go\n"
	st := parseGitStatus([]byte(out))
	if st.Branch == nil || *st.Branch != "main" || st.Detached || !st.HasUpstream || st.Ahead != 3 || st.Behind != 1 || st.DirtyFiles != 5 {
		t.Fatalf("unexpected: %+v", st)
	}
	want := []protocol.GitFile{{Path: "cmd/x.go", Status: "M"}, {Path: "new file.txt", Status: "?"}, {Path: "added.go", Status: "A"}, {Path: "new name.go", Status: "R"}, {Path: "conflict.go", Status: "U"}}
	if !slices.Equal(st.Files, want) {
		t.Fatalf("files: %+v", st.Files)
	}
	st = parseGitStatus([]byte("# branch.oid abc\n# branch.head (detached)\n"))
	if st.Branch != nil || !st.Detached || st.HasUpstream || st.DirtyFiles != 0 {
		t.Fatalf("detached: %+v", st)
	}
}

func TestSpendLedgerTodayAndMonth(t *testing.T) {
	dir := t.TempDir()
	l := newSpendLedger(dir)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.Local)
	l.Record(now, 1.5)
	l.Record(now.AddDate(0, 0, -1), 2)    // same month
	l.Record(now.AddDate(0, -1, 0), 10)   // last month
	sp := newSpendLedger(dir).Totals(now) // reload from disk
	if sp.TodayUSD != 1.5 || sp.MonthUSD != 3.5 {
		t.Fatalf("unexpected: %+v", sp)
	}
}

// A repo opened in an earlier run is watched right after startup, without
// being opened again, so a client sees every repo it has, not just the ones
// it opens.
func TestProjectRegistry_WatchesKnownReposWithoutOpening(t *testing.T) {
	stateDir, repo := t.TempDir(), t.TempDir()
	if err := exec.Command("git", "-C", repo, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(repo); err != nil {
		t.Fatal(err)
	}

	second, err := NewProjectRegistry(nil, "", 0, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	second.SetStatusHub(newStatusHub(NewPeerRegistry(), stateDir))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sts := second.GitStatuses(); len(sts) == 1 {
			if sts[0].DirtyFiles != 1 || len(sts[0].Files) != 1 || sts[0].Files[0].Path != "a.txt" {
				t.Fatalf("status: %+v", sts[0])
			}
			if len(second.open()) != 0 {
				t.Fatal("watching a repo shouldn't open its project")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("known repo never got a git status")
}
