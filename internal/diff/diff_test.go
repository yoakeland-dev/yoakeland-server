package diff_test

import (
	"strings"
	"testing"
	"yoakeland/internal/diff"
)

func TestComputeDiff_NewFile(t *testing.T) {
	d := diff.ComputeDiff("hello.txt", "", "Line 1\nLine 2\n")
	if len(d.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(d.Hunks))
	}
	if !strings.Contains(d.Unified, "+Line 1") || !strings.Contains(d.Unified, "+Line 2") {
		t.Errorf("unexpected unified output:\n%s", d.Unified)
	}
}

func TestComputeDiff_Modification(t *testing.T) {
	before := "func main() {\n\tprintln(\"old\")\n}\n"
	after := "func main() {\n\tprintln(\"new\")\n}\n"

	d := diff.ComputeDiff("main.go", before, after)
	if len(d.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(d.Hunks))
	}

	h := d.Hunks[0]
	hasDelete := false
	hasAdd := false

	for _, l := range h.Lines {
		if l.Type == diff.LineDelete && strings.Contains(l.Content, "old") {
			hasDelete = true
		}
		if l.Type == diff.LineAdd && strings.Contains(l.Content, "new") {
			hasAdd = true
		}
	}

	if !hasDelete || !hasAdd {
		t.Errorf("expected delete and add lines, got: %+v", h.Lines)
	}
}
