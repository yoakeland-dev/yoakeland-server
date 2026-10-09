package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Log, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "audit")
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir
}

func TestAppendAndReadBack(t *testing.T) {
	l, dir := open(t)
	l.Append(Event{Kind: PermDecision, Actor: "laptop", Tool: "bash", Target: "rm -rf build", Outcome: "allow", Session: "s1", Project: "/p", Detail: map[string]any{"remember": true}})
	l.Append(Event{Kind: ModeChange, Actor: "phone", Session: "s1", Outcome: "ok"})

	got, more, err := l.Read(Query{})
	if err != nil || more || len(got) != 2 {
		t.Fatalf("read: %+v more=%v err=%v", got, more, err)
	}
	if got[0].Kind != ModeChange || got[1].Kind != PermDecision {
		t.Fatalf("want newest first: %+v", got)
	}
	if got[1].Seq != 1 || got[0].Seq != 2 || got[1].At.IsZero() || got[1].Actor != "laptop" || got[1].Detail["remember"] != true {
		t.Fatalf("stamped fields: %+v", got[1])
	}

	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", info.Mode())
	}
	if info, _ := os.Stat(filepath.Join(dir, "audit.jsonl")); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", info.Mode())
	}
}

func TestSequenceContinuesAcrossRestarts(t *testing.T) {
	l, dir := open(t)
	l.Append(Event{Kind: PeerJoin})
	l.Append(Event{Kind: PeerJoin})
	_ = l.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.Append(Event{Kind: PeerLeave})
	got, _, _ := again.Read(Query{Limit: 1})
	if len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("seq after reopen: %+v", got)
	}
}

func TestRotationKeepsThreeFilesAndReadSpansThem(t *testing.T) {
	l, dir := open(t)
	l.maxBytes = 600
	for i := 0; i < 60; i++ {
		l.Append(Event{Kind: ToolExec, Tool: "bash", Target: strings.Repeat("x", 40)})
	}
	files, _ := filepath.Glob(filepath.Join(dir, "audit*.jsonl"))
	if len(files) != Keep {
		t.Fatalf("want %d files, got %v", Keep, files)
	}
	for _, f := range files {
		if info, _ := os.Stat(f); info.Size() > 700 {
			t.Fatalf("%s grew past the cap: %d", f, info.Size())
		}
	}
	got, _, err := l.Read(Query{Limit: MaxLimit})
	if err != nil || len(got) == 0 || got[0].Seq != 60 {
		t.Fatalf("newest event lost: %+v %v", got, err)
	}
	// Contiguous and descending across the file boundaries; the oldest are gone.
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq-1 {
			t.Fatalf("gap between %d and %d", got[i-1].Seq, got[i].Seq)
		}
	}
	if got[len(got)-1].Seq == 1 {
		t.Fatal("the oldest file should have been dropped")
	}
}

func TestPagingByBefore(t *testing.T) {
	l, _ := open(t)
	for i := 0; i < 7; i++ {
		l.Append(Event{Kind: ToolExec})
	}
	page1, more, _ := l.Read(Query{Limit: 3})
	if len(page1) != 3 || !more || page1[0].Seq != 7 || page1[2].Seq != 5 {
		t.Fatalf("page 1: %+v more=%v", page1, more)
	}
	page2, more, _ := l.Read(Query{Limit: 3, Before: page1[2].Seq})
	if len(page2) != 3 || !more || page2[0].Seq != 4 {
		t.Fatalf("page 2: %+v more=%v", page2, more)
	}
	page3, more, _ := l.Read(Query{Limit: 3, Before: page2[2].Seq})
	if len(page3) != 1 || more || page3[0].Seq != 1 {
		t.Fatalf("page 3: %+v more=%v", page3, more)
	}
	if big, _, _ := l.Read(Query{Limit: 100000}); len(big) != 7 {
		t.Fatalf("limit is clamped, not an error: %d", len(big))
	}
}

func TestFilters(t *testing.T) {
	l, _ := open(t)
	l.Append(Event{Kind: PermDecision, Session: "a", Project: "/p1"})
	l.Append(Event{Kind: ToolExec, Session: "a", Project: "/p1"})
	l.Append(Event{Kind: ToolExec, Session: "b", Project: "/p2"})
	l.Append(Event{Kind: PeerJoin})

	count := func(q Query) int {
		got, _, err := l.Read(q)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := count(Query{Kinds: []string{ToolExec}}); n != 2 {
		t.Fatalf("kinds: %d", n)
	}
	if n := count(Query{Kinds: []string{ToolExec, PermDecision}}); n != 3 {
		t.Fatalf("two kinds: %d", n)
	}
	if n := count(Query{Session: "a"}); n != 2 {
		t.Fatalf("session: %d", n)
	}
	if n := count(Query{Project: "/p2"}); n != 1 {
		t.Fatalf("project: %d", n)
	}
	if n := count(Query{Session: "a", Kinds: []string{ToolExec}}); n != 1 {
		t.Fatalf("combined: %d", n)
	}
	if n := count(Query{Since: time.Now().Add(time.Hour)}); n != 0 {
		t.Fatalf("since the future: %d", n)
	}
	if n := count(Query{Since: time.Now().Add(-time.Hour)}); n != 4 {
		t.Fatalf("since an hour ago: %d", n)
	}
}

func TestNilLogIsANoOp(t *testing.T) {
	var l *Log
	l.Append(Event{Kind: PermDecision})
	got, more, err := l.Read(Query{})
	if err != nil || more || got == nil || len(got) != 0 {
		t.Fatalf("nil read: %v %v %v", got, more, err)
	}
	if l.Close() != nil {
		t.Fatal("nil close")
	}
}

func TestSecretsAreScrubbed(t *testing.T) {
	l, dir := open(t)
	l.Append(Event{
		Kind:   ToolExec,
		Target: `curl -H "Authorization: Bearer sk-ant-abcdefghijklmnop1234" https://u:hunter2@example.com --token abc123 PASSWORD=hunter2`,
		Detail: map[string]any{
			"apiKey": "sk-live-1234567890abcdef", "password": "hunter2", "authToken": "t",
			"baseUrl": "https://user:s3cret@api.example.com/v1", "note": "ghp_" + strings.Repeat("a", 30),
			"cacheKey": "keep-me",
		},
	})
	raw, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	for _, secret := range []string{"sk-ant-abcdefghijklmnop1234", "hunter2", "abc123", "s3cret", "sk-live-1234567890abcdef", strings.Repeat("a", 30)} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret %q reached the log:\n%s", secret, raw)
		}
	}
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Detail["apiKey"]; ok {
		t.Fatal("a key named like a secret should be dropped, not blanked")
	}
	if e.Detail["cacheKey"] != "keep-me" || !strings.Contains(e.Target, "example.com") {
		t.Fatalf("scrubbing ate innocent text: %+v", e)
	}
}

func TestLongFieldsAreClipped(t *testing.T) {
	l, _ := open(t)
	l.Append(Event{Kind: ToolExec, Target: strings.Repeat("é", 1000), Detail: map[string]any{"err": strings.Repeat("y", 1000)}})
	got, _, _ := l.Read(Query{})
	if n := len([]rune(got[0].Target)); n > maxFieldN+1 {
		t.Fatalf("target %d runes", n)
	}
	if n := len(got[0].Detail["err"].(string)); n > maxFieldN+4 {
		t.Fatalf("detail %d bytes", n)
	}
}

func TestAppendOnlyNeverTruncates(t *testing.T) {
	l, dir := open(t)
	l.Append(Event{Kind: PeerJoin})
	_ = l.Close()
	again, _ := Open(dir)
	defer again.Close()
	again.Append(Event{Kind: PeerLeave})
	got, _, _ := again.Read(Query{})
	if len(got) != 2 {
		t.Fatalf("reopening lost history: %+v", got)
	}
}
