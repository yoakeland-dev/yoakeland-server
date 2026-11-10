package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"yoakeland/internal/tool"
)

// The recall tool (SPEC.md §4g) gives the model back what a chapters
// compaction folded away. It is the agent's own tool, like subagent: it reads
// the session's history, so it lives here rather than in the tool package, and
// the turn loop answers it directly. Its result is an ordinary tool result,
// appended after everything else, so nothing earlier in the context changes
// and the prompt cache stays valid; the next compaction folds it like any
// other message.

const RecallToolName = "recall"

var recallDefinition = tool.ToolDefinition{
	Name: RecallToolName,
	Description: "Bring back earlier parts of this conversation that were folded into chapters. " +
		"Pass keywords to search the full stored messages of every chapter (file names, function names, error text, anything said or shown); " +
		"the best matching chapter is opened in full and other matches are listed. Pass a chapter number to open that chapter directly.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":   map[string]any{"type": "string", "description": "Keywords to search for. Case-insensitive. More specific words give better matches."},
			"chapter": map[string]any{"type": "integer", "description": "Open this chapter in full, skipping the search."},
		},
	},
}

// runRecall answers a recall call against the session's history, copied under
// the lock and searched without it. model sizes the result cap like the
// compaction budget is sized.
func (a *Agent) runRecall(model string, args json.RawMessage) (output, errText string) {
	a.mu.Lock()
	history := append([]Message(nil), a.history...)
	a.mu.Unlock()
	budget := a.contextManager.compactionBudget(a.contextManager.Window(model, history))
	return a.contextManager.recall(history, args, max(budget/4, 4000))
}

// recall searches and opens the chapters of history. capTokens bounds the
// opened chapter's transcript.
func (cm *ContextManager) recall(history []Message, argsJSON json.RawMessage, capTokens int) (output, errText string) {
	var args struct {
		Query   string `json:"query"`
		Chapter *int   `json:"chapter"`
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", "recall takes a query (keywords) or a chapter number"
	}
	fs := foldStateOf(history)
	chapters := fs.chapters
	if len(chapters) == 0 {
		return "", "this conversation has no folded chapters to recall"
	}
	if args.Chapter == nil && strings.TrimSpace(args.Query) == "" {
		return "", "give keywords in query, or a chapter number in chapter"
	}

	recallCalls := map[string]bool{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.Name == RecallToolName {
				recallCalls[tc.ID] = true
			}
		}
	}
	// ponytail: one linear pass over the whole history per call; fine for a
	// chat's size, index the chapters' text once per compaction if it ever isn't.
	byID := make(map[string]int, len(history))
	for i, m := range history {
		byID[m.ID] = i
	}
	rawOf := func(c Chapter) []Message {
		from, ok1 := byID[c.From]
		to, ok2 := byID[c.To]
		if !ok1 || !ok2 || to < from {
			return nil
		}
		var out []Message
		for _, m := range history[from : to+1] {
			if m.Role != "compaction" && !(m.Role == "tool" && recallCalls[m.ToolCallID]) {
				out = append(out, m)
			}
		}
		return out
	}

	pick := -1
	var ranked []recallHit
	if args.Chapter != nil {
		if *args.Chapter < 1 || *args.Chapter > len(chapters) {
			return "", fmt.Sprintf("chapter %d does not exist; the chapters are numbered 1 to %d", *args.Chapter, len(chapters))
		}
		pick = *args.Chapter - 1
	} else {
		var terms []string
		for _, w := range strings.Fields(strings.Map(unicode.ToLower, args.Query)) {
			if utf8.RuneCountInString(w) >= 2 {
				terms = append(terms, w)
			}
		}
		for i, c := range chapters {
			if h := scoreChapter(c, rawOf(c), terms); h.distinct > 0 {
				h.pos = i
				ranked = append(ranked, h)
			}
		}
		slices.SortStableFunc(ranked, func(x, y recallHit) int {
			if x.distinct != y.distinct {
				return y.distinct - x.distinct
			}
			if x.total != y.total {
				return y.total - x.total
			}
			return y.pos - x.pos // newer chapter first
		})
		if len(ranked) == 0 {
			var sb strings.Builder
			fmt.Fprintf(&sb, "No chapter matches %q. The chapters:\n", args.Query)
			for _, c := range chapters {
				fmt.Fprintf(&sb, "- %d: %s\n", c.N, c.Title)
			}
			return sb.String(), ""
		}
		pick = ranked[0].pos
	}

	var sb strings.Builder
	c := chapters[pick]
	if cm.chapterOpen(history, fs, recallCalls, c.N) {
		fmt.Fprintf(&sb, "Chapter %d is already open above.", c.N)
	} else {
		sb.WriteString(cm.openChapter(c, rawOf(c), capTokens))
	}
	var others []recallHit
	for _, h := range ranked {
		if h.pos != pick && len(others) < 5 {
			others = append(others, h)
		}
	}
	if len(others) > 0 {
		sb.WriteString("\n\nOther matches:\n")
		for _, h := range others {
			fmt.Fprintf(&sb, "- Chapter %d: %s — %s\n", chapters[h.pos].N, chapters[h.pos].Title, h.snippet)
		}
	}
	return sb.String(), ""
}

// chapterOpen reports whether a recall result for chapter n is still in the
// part of the history the model sees, not folded and not cleared.
func (cm *ContextManager) chapterOpen(history []Message, fs foldState, recallCalls map[string]bool, n int) bool {
	for i := max(fs.start, fs.clearIdx); i < len(history); i++ {
		m := history[i]
		var got int
		if m.Role == "tool" && recallCalls[m.ToolCallID] {
			if _, err := fmt.Sscanf(m.Content, "[Chapter %d:", &got); err == nil && got == n {
				return true
			}
		}
	}
	return false
}

// openChapter is the chapter's transcript under a header. A transcript over
// capTokens is rendered again with every tool result cut to its head and tail,
// at smaller and smaller sizes, until it fits.
func (cm *ContextManager) openChapter(c Chapter, msgs []Message, capTokens int) string {
	header := fmt.Sprintf("[Chapter %d: %s — unfolded, messages %s–%s]\n", c.N, c.Title, c.From, c.To)
	if len(msgs) == 0 {
		return header + "(the stored messages of this chapter are not available)\n\nBrief: " + c.Brief
	}
	full := func(s string) string { return s }
	text := writeTranscript(msgs, full, full)
	if cm.EstimateTokens(text) <= capTokens {
		return header + text
	}
	note := "[Long tool results were shortened to fit. Run the tool again to get one in full.]\n"
	for _, n := range []int{2000, 500, 150} {
		text = writeTranscript(msgs, full, func(s string) string { return headTail(s, n*4) })
		if cm.EstimateTokens(text) <= capTokens {
			return header + note + text
		}
	}
	// Even the shortest results leave more than fits: keep the start.
	return header + note + truncateRunes(text, capTokens*4)
}

// headTail keeps the first and last half of the allowed runes of s and says how
// much was cut between.
func headTail(s string, runes int) string {
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	half := runes / 2
	return string(r[:half]) + fmt.Sprintf("\n[... %d characters cut ...]\n", len(r)-2*half) + string(r[len(r)-half:])
}

// recallHit is how well one chapter matches a search.
type recallHit struct {
	pos      int // index in the chapter list
	distinct int // search terms found
	total    int // occurrences, each term counted at most 5 times
	snippet  string
}

// scoreChapter counts terms in the chapter's title, brief and stored messages
// (text, tool call names and arguments, tool results; earlier recall calls are
// left out so a reopened copy can't outrank the original).
func scoreChapter(c Chapter, msgs []Message, terms []string) recallHit {
	var sb strings.Builder
	sb.WriteString(c.Title + "\n" + c.Brief + "\n")
	for _, m := range msgs {
		sb.WriteString(m.Content + "\n")
		for _, tc := range m.ToolCalls {
			if tc.Name != RecallToolName {
				sb.WriteString(tc.Name + " " + string(tc.Args) + "\n")
			}
		}
	}
	text := sb.String()
	lower := strings.Map(unicode.ToLower, text)
	var h recallHit
	first := -1
	for _, t := range terms {
		n := strings.Count(lower, t)
		if n == 0 {
			continue
		}
		h.distinct++
		h.total += min(n, 5)
		if at := strings.Index(lower, t); first < 0 || at < first {
			first = at
		}
	}
	if first >= 0 {
		// ToLower keeps the rune count, so a rune offset into lower is the
		// same place in text.
		at := utf8.RuneCountInString(lower[:first])
		r := []rune(text)
		h.snippet = strings.Join(strings.Fields(string(r[max(at-50, 0):min(at+110, len(r))])), " ")
	}
	return h
}
