package diff

import (
	"fmt"
	"strings"
)

type LineType string

const (
	LineContext LineType = " "
	LineAdd     LineType = "+"
	LineDelete  LineType = "-"
)

type DiffLine struct {
	Type    LineType `json:"type"`
	Content string   `json:"content"`
}

type Hunk struct {
	OldStart int        `json:"oldStart"`
	OldLines int        `json:"oldLines"`
	NewStart int        `json:"newStart"`
	NewLines int        `json:"newLines"`
	Lines    []DiffLine `json:"lines"`
}

type FileDiff struct {
	FilePath string `json:"filePath"`
	Unified  string `json:"unified"`
	Hunks    []Hunk `json:"hunks"`
}

// ComputeDiff runs the Myers diff algorithm between two texts and builds structured hunks
func ComputeDiff(filePath, before, after string) *FileDiff {
	oldLines := splitLines(before)
	newLines := splitLines(after)

	edits := myersDiff(oldLines, newLines)
	hunks := buildHunks(edits, 3) // 3 lines of surrounding context

	unified := formatUnified(filePath, hunks)

	return &FileDiff{
		FilePath: filePath,
		Unified:  unified,
		Hunks:    hunks,
	}
}

// MaxPreviewBytes caps each side of a permission preview. A larger change is
// approved without one: its diff, which carries every changed line twice (as
// hunks and as unified text), can exceed the IPC frame limit, and the client
// then drops the connection while the turn waits on an answer it can't see.
// At this size, even control characters (escaped to six bytes in JSON) keep
// the frame under protocol.MaxFrameSize.
const MaxPreviewBytes = 256 << 10

// ComputePreview is ComputeDiff for a permission prompt: nil when either side
// is over MaxPreviewBytes.
func ComputePreview(filePath, before, after string) *FileDiff {
	if len(before) > MaxPreviewBytes || len(after) > MaxPreviewBytes {
		return nil
	}
	return ComputeDiff(filePath, before, after)
}

func splitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	// If text ends with a trailing newline, drop the trailing empty element
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type editOp struct {
	op   LineType // " ", "+", "-"
	text string
}

func myersDiff(a, b []string) []editOp {
	n := len(a)
	m := len(b)
	max := n + m

	if max == 0 {
		return nil
	}

	v := make(map[int]int)
	v[1] = 0

	trace := make([]map[int]int, 0, min(max, maxEditDistance)+1)

	for d := 0; d <= max; d++ {
		if d > maxEditDistance {
			return replaceAll(a, b)
		}
		vCopy := make(map[int]int, len(v))
		for k, val := range v {
			vCopy[k] = val
		}
		trace = append(trace, vCopy)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k

			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k] = x

			if x >= n && y >= m {
				return backtrack(trace, a, b, n, m)
			}
		}
	}

	return nil
}

// maxEditDistance bounds myersDiff: its trace keeps O(d²) entries, so a
// wholesale rewrite of a big file would otherwise eat memory. Past it the
// diff is reported as every old line removed and every new line added.
const maxEditDistance = 2000

func replaceAll(a, b []string) []editOp {
	edits := make([]editOp, 0, len(a)+len(b))
	for _, l := range a {
		edits = append(edits, editOp{op: LineDelete, text: l})
	}
	for _, l := range b {
		edits = append(edits, editOp{op: LineAdd, text: l})
	}
	return edits
}

func backtrack(trace []map[int]int, a, b []string, n, m int) []editOp {
	var edits []editOp
	x := n
	y := m

	for d := len(trace) - 1; d > 0; d-- {
		v := trace[d]
		k := x - y

		var prevK int
		if k == -d || (k != d && v[k-1] < v[k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}

		prevX := v[prevK]
		prevY := prevX - prevK

		for x > prevX && y > prevY {
			edits = append(edits, editOp{op: LineContext, text: a[x-1]})
			x--
			y--
		}

		if d > 0 {
			if x > prevX {
				edits = append(edits, editOp{op: LineDelete, text: a[x-1]})
				x--
			} else if y > prevY {
				edits = append(edits, editOp{op: LineAdd, text: b[y-1]})
				y--
			}
		}
	}

	for x > 0 && y > 0 {
		edits = append(edits, editOp{op: LineContext, text: a[x-1]})
		x--
		y--
	}
	for x > 0 {
		edits = append(edits, editOp{op: LineDelete, text: a[x-1]})
		x--
	}
	for y > 0 {
		edits = append(edits, editOp{op: LineAdd, text: b[y-1]})
		y--
	}

	// Reverse to restore forward chronological order
	for i, j := 0, len(edits)-1; i < j; i, j = i+1, j-1 {
		edits[i], edits[j] = edits[j], edits[i]
	}

	return edits
}

// buildHunks groups edits into one or more windowed hunks, each trimmed to
// at most contextLines of unchanged context before/after its changes —
// rather than one hunk spanning the entire file — merging two changes into
// the same hunk when the run of context between them is short enough that
// their context windows would otherwise overlap.
func buildHunks(edits []editOp, contextLines int) []Hunk {
	if len(edits) == 0 {
		return nil
	}

	// oldLineAt[i]/newLineAt[i] are the 1-based old/new line numbers "at"
	// edits[i] — i.e. the position a hunk starting at edits[i] should report
	// as its OldStart/NewStart.
	oldLineAt := make([]int, len(edits))
	newLineAt := make([]int, len(edits))
	oldLine, newLine := 1, 1
	for i, e := range edits {
		oldLineAt[i], newLineAt[i] = oldLine, newLine
		switch e.op {
		case LineContext:
			oldLine++
			newLine++
		case LineDelete:
			oldLine++
		case LineAdd:
			newLine++
		}
	}

	var changeIdx []int
	for i, e := range edits {
		if e.op != LineContext {
			changeIdx = append(changeIdx, i)
		}
	}
	if len(changeIdx) == 0 {
		return nil
	}

	// Merge adjacent changes into one range when the context run between
	// them is short enough that both changes' context windows would touch.
	type editRange struct{ start, end int } // inclusive indices into edits
	var ranges []editRange
	curStart, curEnd := changeIdx[0], changeIdx[0]
	for _, idx := range changeIdx[1:] {
		gap := idx - curEnd - 1
		if gap <= 2*contextLines {
			curEnd = idx
			continue
		}
		ranges = append(ranges, editRange{curStart, curEnd})
		curStart, curEnd = idx, idx
	}
	ranges = append(ranges, editRange{curStart, curEnd})

	hunks := make([]Hunk, 0, len(ranges))
	for _, r := range ranges {
		lo := r.start - contextLines
		if lo < 0 {
			lo = 0
		}
		hi := r.end + contextLines
		if hi > len(edits)-1 {
			hi = len(edits) - 1
		}

		lines := make([]DiffLine, 0, hi-lo+1)
		var oldCount, newCount int
		for i := lo; i <= hi; i++ {
			e := edits[i]
			lines = append(lines, DiffLine{Type: e.op, Content: e.text})
			switch e.op {
			case LineContext:
				oldCount++
				newCount++
			case LineDelete:
				oldCount++
			case LineAdd:
				newCount++
			}
		}

		hunks = append(hunks, Hunk{
			OldStart: oldLineAt[lo],
			OldLines: oldCount,
			NewStart: newLineAt[lo],
			NewLines: newCount,
			Lines:    lines,
		})
	}

	return hunks
}

func formatUnified(filePath string, hunks []Hunk) string {
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n", filePath))
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))

	for _, h := range hunks {
		sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines))
		for _, l := range h.Lines {
			sb.WriteString(fmt.Sprintf("%s%s\n", l.Type, l.Content))
		}
	}

	return sb.String()
}
