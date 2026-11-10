package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"yoakeland/internal/protocol"
)

// Smart compaction (SPEC.md §4g): instead of one summary standing in for
// everything, the older conversation is cut into chapters, each with a title
// and a brief written by the utility model. The model sees the briefs, and the
// recall tool (recall.go) reopens a chapter's full messages. History keeps
// every message; a chapters compaction only records how the view is cut.

// Chapter is one folded stretch of the conversation. It is the wire type: the
// stored compaction and the context_compacted event carry the same fields.
type Chapter = protocol.ChapterInfo

// chapterIndexOpening starts the user message that lists the chapters. The
// context breakdown recognizes it by this prefix.
const chapterIndexOpening = "[Earlier parts of this conversation are folded into chapters to save context. "

const chapterIndexIntro = chapterIndexOpening +
	"Each chapter's brief is below. Nothing was lost: the full messages are stored. " +
	"Call the recall tool with keywords to search every folded chapter and open the best match in full, " +
	"or with a chapter number to open that one. Use it whenever a brief is not enough " +
	"(exact code, command output, wording) instead of guessing or redoing work.]"

const chapterBriefPrompt = `You write the brief for one chapter of a coding agent's session. The chapter's full messages stay stored and searchable, so this is an index entry, not a replacement: say what happened so the agent knows what is in here and when to open it.
First line: a title of at most 8 words, no trailing punctuation.
Then the brief, at most 200 words: what the user asked for and their constraints, what was decided and why, files read or changed with paths, commands run and their outcome, errors and how they were resolved, and anything left unfinished. Use exact names, paths and values, since those are what the agent will search for. No preamble.`

// foldState is what the recorded compactions say about a history: where the
// unfolded part starts and what stands in for the part before it. It reads
// only recorded fields, never the budget or the clock.
type foldState struct {
	start      int    // first history index the view replays
	clearIdx   int    // tool results before this index are stubbed; -1 = none
	last       int    // index of the latest compaction message; -1 = none
	summary    string // a legacy summary, shown before the chapters
	chapters   []Chapter
	titlesOnly int
	request    string
}

func foldStateOf(history []Message) foldState {
	fs := foldState{clearIdx: -1, last: -1}
	for i, m := range history {
		if m.Role != "compaction" || m.Compaction == nil {
			continue
		}
		fs.last = i
		c := m.Compaction
		switch c.Kind {
		case CompactionSummary:
			fs = foldState{start: i + 1, clearIdx: -1, last: i, summary: m.Content}
		case CompactionClear:
			if j, ok := indexOfMessage(history, c.ClearBefore); ok && j > fs.clearIdx {
				fs.clearIdx = j
			}
		case CompactionChapters:
			fs.start = i + 1
			if j, ok := indexOfMessage(history, c.KeepFrom); ok && c.KeepFrom != "" {
				fs.start = j
			}
			fs.chapters = append(fs.chapters[:len(fs.chapters):len(fs.chapters)], c.Chapters...)
			fs.titlesOnly, fs.request = c.TitlesOnly, c.Request
		}
	}
	return fs
}

// head is the user message that opens the view in place of the folded part:
// the legacy summary if there is one, then the chapter index. "" when nothing
// was folded.
func (fs foldState) head() string {
	var sb strings.Builder
	if fs.summary != "" {
		sb.WriteString(summaryOpening + fs.summary)
	}
	if len(fs.chapters) == 0 {
		return sb.String()
	}
	if sb.Len() > 0 {
		sb.WriteString("\n\n")
	}
	sb.WriteString(chapterIndexIntro)
	// ponytail: every chapter keeps one title line in the index forever
	// (TitlesOnly only drops briefs). Merge the oldest chapters into volumes
	// when chats get long enough for the titles themselves to matter.
	for i, c := range fs.chapters {
		fmt.Fprintf(&sb, "\n\n## Chapter %d: %s\n", c.N, c.Title)
		if i < fs.titlesOnly {
			fmt.Fprintf(&sb, "(brief folded; recall chapter %d to read it)", c.N)
		} else {
			sb.WriteString(c.Brief)
		}
	}
	if fs.request != "" {
		sb.WriteString("\n\n[The request you are working on, verbatim:]\n" + fs.request)
	}
	return sb.String()
}

// renderRange is history[from:to] as the model sees it: compaction messages
// left out, old tool results stubbed, RawContent kept only after the last
// compaction.
func renderRange(history []Message, fs foldState, from, to int) []Message {
	skillResults := skillResultIDs(history)
	out := make([]Message, 0, to-from)
	for i := from; i < to; i++ {
		m := history[i]
		if m.Role == "compaction" {
			continue
		}
		if i < fs.clearIdx && !(m.Role == "tool" && skillResults[m.ToolCallID]) {
			m = stubbed(m)
		}
		if i < fs.last {
			m.RawContent = nil
		}
		out = append(out, m)
	}
	return out
}

// foldable is history[from:to] as stored, without compaction messages and
// without RawContent, which a fold never replays.
func foldable(history []Message, from, to int) []Message {
	var out []Message
	for _, m := range history[from:to] {
		if m.Role != "compaction" {
			m.RawContent = nil
			out = append(out, m)
		}
	}
	return out
}

// hasChapters reports whether the model's view has folded chapters, which is
// when it is offered the recall tool.
func (a *Agent) hasChapters() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(foldStateOf(a.history).chapters) > 0
}

// tailStart is where the unfolded tail of a chapters compaction begins: the
// start of a round (a user or assistant message, never a tool result), going
// back over whole rounds while the tail stays within about tokens. The latest
// round is always kept. len(history) means nothing is kept.
func (cm *ContextManager) tailStart(history []Message, fs foldState, tokens int) int {
	keep, size, round := len(history), 0, 0
	for i := len(history) - 1; i >= fs.start; i-- {
		m := history[i]
		if m.Role == "compaction" {
			continue
		}
		if i < fs.clearIdx {
			m = stubbed(m) // the tail is sized as the model sees it
		}
		round += cm.EstimateHistoryTokens([]Message{m})
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if keep < len(history) && size+round > tokens {
			break
		}
		keep, size, round = i, size+round, 0
	}
	return keep
}

// segmentChapters cuts msgs into chapters at round starts: before a user
// message once the segment is past half the target, before an assistant
// message once it is past the target. Tool results stay with their call, and
// one round bigger than the target is one chapter.
func (cm *ContextManager) segmentChapters(msgs []Message, budget int) [][]Message {
	target := min(max(budget/8, 3000), 12000)
	var segs [][]Message
	from, size := 0, 0
	for i, m := range msgs {
		if i > from && ((m.Role == "user" && size >= target/2) || (m.Role == "assistant" && size >= target)) {
			segs = append(segs, msgs[from:i])
			from, size = i, 0
		}
		size += cm.EstimateHistoryTokens([]Message{m})
	}
	if from < len(msgs) {
		segs = append(segs, msgs[from:])
	}
	return segs
}

// splitBrief turns a model's answer into a title (its first line, 80 runes at
// most) and a brief (the rest). Either is empty when the answer has none.
func splitBrief(answer string) (title, brief string) {
	first, rest, _ := strings.Cut(strings.TrimSpace(answer), "\n")
	title = strings.TrimRight(strings.Trim(first, "# *\t"), ".:;")
	return truncateRunes(title, 80), strings.TrimSpace(rest)
}

// fallbackBrief is the chapter entry for a segment the utility model couldn't
// brief: the first thing the user said in it, and the conversation's own words.
func fallbackBrief(seg []Message) (title, brief string) {
	title = "Earlier work"
	for _, m := range seg {
		if line, _, _ := strings.Cut(strings.TrimSpace(m.Content), "\n"); m.Role == "user" && line != "" {
			title = truncateRunes(line, 80)
			break
		}
	}
	return title, truncateRunes(fallbackSummary(seg), 800)
}

// writeBriefs asks the utility model for each segment's title and brief, at
// most four requests at once. A segment whose request fails or comes back
// empty, or any segment when there is no API model to ask, gets the
// deterministic fallback, so a chapters compaction never fails. Only a
// cancelled ctx is an error, since the turn is over anyway.
func writeBriefs(ctx context.Context, b RoleBinding, segs [][]Message, instructions string) (titles, briefs []string, usage protocol.UsageData, err error) {
	titles, briefs = make([]string, len(segs)), make([]string, len(segs))
	_, native := b.Provider.(NativeAgent)
	system := withGuidance(chapterBriefPrompt, instructions)
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, 4)
	)
	for i, seg := range segs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if !native && b.Provider != nil {
				answer, u, rerr := requestSummary(ctx, b, seg, system)
				mu.Lock()
				addUsage(&usage, u)
				usage.CostUSD += u.CostUSD
				mu.Unlock()
				if t, br := splitBrief(answer); rerr == nil && t != "" && br != "" {
					titles[i], briefs[i] = t, br
					return
				}
			}
			titles[i], briefs[i] = fallbackBrief(seg)
		}()
	}
	wg.Wait()
	return titles, briefs, usage, ctx.Err()
}

// fitTitles raises titlesOnly (oldest chapter first) until the briefs still
// shown fit in limit tokens.
func (cm *ContextManager) fitTitles(chapters []Chapter, titlesOnly, limit int) int {
	total := 0
	for _, c := range chapters[min(titlesOnly, len(chapters)):] {
		total += cm.EstimateTokens(c.Brief)
	}
	for ; total > limit && titlesOnly < len(chapters); titlesOnly++ {
		total -= cm.EstimateTokens(chapters[titlesOnly].Brief)
	}
	return titlesOnly
}

// compactByChapters records a chapters compaction: the conversation older than
// a tail of about budget/4 tokens (all of it for a manual one) is cut into
// chapters and briefed (SPEC.md §4g). done is false, with nothing recorded,
// when there is nothing to fold, so the caller falls back to a summary.
// The error is only a cancelled ctx.
func (a *Agent) compactByChapters(ctx context.Context, turnID string, bound RoleBinding, history []Message, used, overhead, budget int, extra string, manual bool) (usage protocol.UsageData, done bool, err error) {
	cm := a.contextManager
	fs := foldStateOf(history)
	keep := len(history)
	if !manual {
		keep = cm.tailStart(history, fs, budget/4)
		// A tail that is still over half the budget (one huge latest round)
		// would have the next round compact again, and an empty fold range
		// would push the caller to a summary that wipes the chapters: fold
		// everything instead; the request is kept verbatim in the index.
		if cm.EstimateHistoryTokens(renderRange(history, fs, keep, len(history))) > budget/2 || len(foldable(history, fs.start, keep)) == 0 {
			keep = len(history)
		}
	}
	// Briefs and sizes come from the stored messages, not the clear stubs:
	// the writer needs what happened, and recall restores these.
	msgs := foldable(history, fs.start, keep)
	if len(msgs) == 0 {
		return usage, false, nil
	}
	segs := cm.segmentChapters(msgs, budget)

	summarizer := a.roleOr(RoleUtility, bound)
	instructions := strings.TrimSpace(compactInstructions() + "\n" + extra)
	titles, briefs, usage, err := writeBriefs(ctx, summarizer, segs, instructions)
	if err != nil {
		return usage, false, err
	}

	chapters := make([]Chapter, len(segs))
	for i, seg := range segs {
		chapters[i] = Chapter{
			N: len(fs.chapters) + i + 1, Title: titles[i], Brief: briefs[i],
			From: seg[0].ID, To: seg[len(seg)-1].ID, Tokens: cm.EstimateHistoryTokens(seg),
		}
	}
	c := &Compaction{
		Kind:         CompactionChapters,
		Chapters:     chapters,
		TitlesOnly:   cm.fitTitles(append(fs.chapters[:len(fs.chapters):len(fs.chapters)], chapters...), fs.titlesOnly, budget/4),
		TokensBefore: used,
	}
	if keep < len(history) {
		c.KeepFrom = history[keep].ID
	}
	// The request is shown in the index only when the user's latest message
	// is no longer in the unfolded part.
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			if i < keep {
				c.Request = history[i].Content
			}
			break
		}
	}
	trial := append(history[:len(history):len(history)], Message{Role: "compaction", Compaction: c})
	c.TokensAfter = overhead + cm.EstimateHistoryTokens(contextView(trial, ""))

	a.recordCompaction(turnID, Message{
		Role:       "compaction",
		Provider:   summarizer.ProviderName,
		Model:      summarizer.Model,
		Usage:      &usage,
		Compaction: c,
	}, manual)
	return usage, true, nil
}
