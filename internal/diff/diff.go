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

	trace := make([]map[int]int, 0, max+1)

	for d := 0; d <= max; d++ {
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

func buildHunks(edits []editOp, contextLines int) []Hunk {
	_ = contextLines
	if len(edits) == 0 {
		return nil
	}

	// Check if there are any actual changes
	hasChanges := false
	for _, e := range edits {
		if e.op == LineAdd || e.op == LineDelete {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		return nil
	}

	var hunks []Hunk
	var currentLines []DiffLine

	oldLine := 1
	newLine := 1
	hunkOldStart := 1
	hunkNewStart := 1
	hunkOldCount := 0
	hunkNewCount := 0

	for _, e := range edits {
		diffLine := DiffLine{Type: e.op, Content: e.text}

		if len(currentLines) == 0 {
			hunkOldStart = oldLine
			hunkNewStart = newLine
		}

		currentLines = append(currentLines, diffLine)

		switch e.op {
		case LineContext:
			oldLine++
			newLine++
			hunkOldCount++
			hunkNewCount++
		case LineDelete:
			oldLine++
			hunkOldCount++
		case LineAdd:
			newLine++
			hunkNewCount++
		}
	}

	if len(currentLines) > 0 {
		hunks = append(hunks, Hunk{
			OldStart: hunkOldStart,
			OldLines: hunkOldCount,
			NewStart: hunkNewStart,
			NewLines: hunkNewCount,
			Lines:    currentLines,
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
