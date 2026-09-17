package diff_test

import (
	"fmt"
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

// TestComputeDiff_LargeFileSmallChange guards against a one-line change in a
// large file producing a single hunk that spans the entire file (the bug:
// buildHunks accepted a contextLines parameter but discarded it) — the hunk
// must instead be windowed to a small number of context lines around the
// change, matching how a real unified diff behaves.
func TestComputeDiff_LargeFileSmallChange(t *testing.T) {
	const numLines = 1000
	var beforeLines, afterLines []string
	for i := 1; i <= numLines; i++ {
		line := fmt.Sprintf("line %d", i)
		beforeLines = append(beforeLines, line)
		if i == 500 {
			afterLines = append(afterLines, "line 500 CHANGED")
		} else {
			afterLines = append(afterLines, line)
		}
	}
	before := strings.Join(beforeLines, "\n") + "\n"
	after := strings.Join(afterLines, "\n") + "\n"

	d := diff.ComputeDiff("big.txt", before, after)
	if len(d.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(d.Hunks))
	}

	h := d.Hunks[0]
	// 1 deleted + 1 added + up to 3 lines of context on each side = at most 8
	// lines — nowhere near the full 1000-line file.
	if len(h.Lines) > 10 {
		t.Fatalf("expected a small windowed hunk, got %d lines (contextLines is being ignored again)", len(h.Lines))
	}
	if h.OldStart < 495 || h.OldStart > 500 {
		t.Errorf("expected OldStart near line 500, got %d", h.OldStart)
	}
}

// TestComputeDiff_TwoDistantChanges_ProduceSeparateHunks guards the merging
// logic: two changes far apart in a large file must produce two independent
// windowed hunks, not one hunk spanning (or merging across) the whole file.
func TestComputeDiff_TwoDistantChanges_ProduceSeparateHunks(t *testing.T) {
	const numLines = 1000
	var beforeLines, afterLines []string
	for i := 1; i <= numLines; i++ {
		line := fmt.Sprintf("line %d", i)
		beforeLines = append(beforeLines, line)
		if i == 100 {
			afterLines = append(afterLines, "line 100 CHANGED")
		} else if i == 900 {
			afterLines = append(afterLines, "line 900 CHANGED")
		} else {
			afterLines = append(afterLines, line)
		}
	}
	before := strings.Join(beforeLines, "\n") + "\n"
	after := strings.Join(afterLines, "\n") + "\n"

	d := diff.ComputeDiff("big.txt", before, after)
	if len(d.Hunks) != 2 {
		t.Fatalf("expected 2 separate hunks for two distant changes, got %d", len(d.Hunks))
	}
	for _, h := range d.Hunks {
		if len(h.Lines) > 10 {
			t.Errorf("expected each hunk to be small/windowed, got %d lines", len(h.Lines))
		}
	}
}
