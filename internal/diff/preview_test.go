package diff_test

import (
	"encoding/json"
	"strings"
	"testing"

	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
)

// A preview is dropped, not sent, when a side is too big, and the largest
// one still fits a frame even when every character is escaped on the wire.
func TestComputePreview_CapsSizeSoTheFrameFits(t *testing.T) {
	if d := diff.ComputePreview("a.txt", "small\n", "small\nmore\n"); d == nil {
		t.Fatal("a small change must still get a preview")
	}
	big := strings.Repeat("x", diff.MaxPreviewBytes+1)
	if d := diff.ComputePreview("a.txt", "", big); d != nil {
		t.Fatal("a change over the cap must have no preview")
	}

	worst := diff.ComputePreview("a.txt", strings.Repeat("\x01", diff.MaxPreviewBytes), strings.Repeat("\x02", diff.MaxPreviewBytes))
	if worst == nil {
		t.Fatal("a change at the cap must still get a preview")
	}
	data, err := json.Marshal(struct {
		Type string
		Diff *diff.FileDiff
	}{"permission_request", worst})
	if err != nil {
		t.Fatal(err)
	}
	if len(data)+1 > protocol.MaxFrameSize {
		t.Fatalf("largest preview encodes to %d bytes, over the %d-byte frame", len(data), protocol.MaxFrameSize)
	}
}
