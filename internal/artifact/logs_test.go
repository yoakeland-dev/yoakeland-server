package artifact

import (
	"strings"
	"testing"
)

func TestLogsBoundsAndDefaultArtifact(t *testing.T) {
	l := &Logs{entries: map[string]map[string][]Entry{}, order: map[string][]string{}}
	if !strings.Contains(l.Read("s", ""), "nothing logged") {
		t.Fatal("empty read")
	}
	for i := 0; i < maxEntries+10; i++ {
		l.Add("s", "a1", "log", "line")
	}
	l.Add("s", "a2", "error", strings.Repeat("x", maxEntryLen*2))
	if got := l.Read("s", "a1"); !strings.Contains(got, "200 entries") {
		t.Fatalf("ring not bounded: %.60s", got)
	}
	got := l.Read("s", "") // the latest artifact
	if !strings.Contains(got, "artifact a2") || strings.Count(got, "x") > maxEntryLen {
		t.Fatalf("default or truncation wrong: %.80s", got)
	}
	for i := 0; i < maxArtifacts+5; i++ {
		l.Add("s2", string(rune('a'+i%26))+string(rune('a'+i/26)), "log", "x")
	}
	if len(l.entries["s2"]) != maxArtifacts {
		t.Fatalf("artifacts kept: %d", len(l.entries["s2"]))
	}
	l.Forget("s")
	if !strings.Contains(l.Read("s", "a1"), "nothing logged") {
		t.Fatal("Forget kept logs")
	}
}
