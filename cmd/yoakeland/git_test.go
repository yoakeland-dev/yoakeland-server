package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	first, err := NewProjectRegistry(nil, "", 0, stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(repo); err != nil {
		t.Fatal(err)
	}

	second, err := NewProjectRegistry(nil, "", 0, stateDir, "")
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

func TestGitChanges_NotARepoSaysSo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	_, _, err := gitChanges(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("err = %v, want git's own \"not a git repository\"", err)
	}
}

// A .git/config written from inside the sandbox must not run on the host
// when yoakeland itself runs git status.
func TestHostGitIgnoresFsmonitor(t *testing.T) {
	repo := t.TempDir()
	if err := exec.Command("git", "-C", repo, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	marker := filepath.Join(repo, "PWNED")
	if err := exec.Command("git", "-C", repo, "config", "core.fsmonitor", "touch '"+marker+"'; false #").Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitChanges(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.fsmonitor ran on the host")
	}
}

// A project inside a larger repo reads its own HEAD copies, and a path that
// climbs out of it is refused instead of read from a sibling directory.
func TestGitShowHead_SubProjectStaysInside(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for p, s := range map[string]string{"a/s.txt": "mine", "b/s.txt": "sibling"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	sub := filepath.Join(repo, "a")
	if got, _, err := gitShowHead(sub, "s.txt"); err != nil || string(got) != "mine" {
		t.Fatalf("own file at HEAD: %q, %v", got, err)
	}
	for _, rel := range []string{"../b/s.txt", "x/../../b/s.txt", "/etc/passwd"} {
		if got, _, err := gitShowHead(sub, rel); err == nil {
			t.Fatalf("%s read %q", rel, got)
		}
	}
}
