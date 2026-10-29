package agent

import "testing"

func TestParseSummary(t *testing.T) {
	s, ask, a := parseSummary("```json\n{\"summary\":\"Did it.\",\"ask\":\"Want me to run them?\",\"actions\":[{\"label\":\"Run tests\",\"prompt\":\"Run the full suite and report.\"},{\"label\":\"\",\"prompt\":\"x\"}]}\n```")
	if s != "Did it." || ask != "Want me to run them?" || len(a) != 1 || a[0].Label != "Run tests" {
		t.Fatalf("got %q %q %v", s, ask, a)
	}
	if s, ask, a := parseSummary("Just prose, no JSON."); s != "Just prose, no JSON." || ask != "" || a != nil {
		t.Fatalf("plain text lost: %q %q %v", s, ask, a)
	}
	if _, ask, _ := parseSummary(`{"summary":"x","ask":"Orphan question?","actions":[]}`); ask != "" {
		t.Fatal("a question with no buttons should be dropped")
	}
}
