package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// round is a user question, an assistant call and its result, and an answer:
// four messages with ids r<i>u, r<i>c, r<i>t, r<i>a.
func round(i int, result string) []Message {
	id := fmt.Sprintf("r%d", i)
	return []Message{
		{ID: id + "u", Role: "user", Content: fmt.Sprintf("question %d", i)},
		{ID: id + "c", Role: "assistant", ToolCalls: []ToolCall{{ID: id + "x", Name: "bash", Args: json.RawMessage(fmt.Sprintf(`{"cmd":"run %d"}`, i))}}},
		{ID: id + "t", Role: "tool", ToolCallID: id + "x", Content: result},
		{ID: id + "a", Role: "assistant", Content: fmt.Sprintf("answer %d", i)},
	}
}

func rounds(n int, result string) []Message {
	var h []Message
	for i := 1; i <= n; i++ {
		h = append(h, round(i, result)...)
	}
	return h
}

func chaptersCompaction(id, keepFrom string, titlesOnly int, request string, chapters ...Chapter) Message {
	return Message{ID: id, Role: "compaction", Compaction: &Compaction{
		Kind: CompactionChapters, Chapters: chapters, KeepFrom: keepFrom, TitlesOnly: titlesOnly, Request: request,
	}}
}

func ch(n int, title, from, to string) Chapter {
	return Chapter{N: n, Title: title, Brief: "brief of " + title, From: from, To: to, Tokens: 10}
}

func roles(view []Message) string {
	var sb strings.Builder
	for _, m := range view {
		sb.WriteString(m.Role[:1])
	}
	return sb.String()
}

func TestChapterView_ReplacesFoldedRangeAndAccumulates(t *testing.T) {
	h := rounds(4, "ok")
	h = append(h, chaptersCompaction("k1", "r3u", 0, "", ch(1, "Setup", "r1u", "r1a"), ch(2, "Parser", "r2u", "r2a")))
	view := contextView(h, "")
	if len(view) != 1+8 || !strings.HasPrefix(view[0].Content, chapterIndexOpening) || view[1].ID != "r3u" {
		t.Fatalf("view = %d messages, first %q", len(view), view[0].Content)
	}
	for _, want := range []string{"## Chapter 1: Setup\nbrief of Setup", "## Chapter 2: Parser\nbrief of Parser", "recall tool"} {
		if !strings.Contains(view[0].Content, want) {
			t.Fatalf("index lacks %q:\n%s", want, view[0].Content)
		}
	}

	// A second compaction adds its chapters to the first one's.
	h = append(h, chaptersCompaction("k2", "r4u", 0, "", ch(3, "Tests", "r3u", "r3a")))
	view = contextView(h, "")
	if len(view) != 1+4 || view[1].ID != "r4u" || !strings.Contains(view[0].Content, "## Chapter 3: Tests") || !strings.Contains(view[0].Content, "## Chapter 1: Setup") {
		t.Fatalf("chapters did not accumulate: %q", view[0].Content)
	}

	// A summary starts over, chapter list included.
	h = append(h, Message{ID: "s", Role: "compaction", Content: "the whole story", Compaction: &Compaction{Kind: CompactionSummary}})
	view = contextView(h, "")
	if len(view) != 1 || view[0].Content != summaryOpening+"the whole story" {
		t.Fatalf("summary should reset the chapters: %+v", view)
	}
	// Chapters made after a summary follow it in the same message.
	h = append(h, round(5, "ok")...)
	h = append(h, chaptersCompaction("k3", "", 0, "", ch(1, "After", "r5u", "r5a")))
	view = contextView(h, "")
	if len(view) != 1 || !strings.HasPrefix(view[0].Content, summaryOpening+"the whole story\n\n"+chapterIndexOpening) || !strings.Contains(view[0].Content, "## Chapter 1: After") {
		t.Fatalf("legacy summary should precede the index: %q", view[0].Content)
	}
}

func TestChapterView_TitlesOnlyAndRequest(t *testing.T) {
	h := rounds(2, "ok")
	h = append(h, chaptersCompaction("k", "", 1, "fix the parser please", ch(1, "Setup", "r1u", "r1a"), ch(2, "Parser", "r2u", "r2a")))
	got := contextView(h, "")[0].Content
	if strings.Contains(got, "brief of Setup") || !strings.Contains(got, "## Chapter 1: Setup\n(brief folded; recall chapter 1 to read it)") {
		t.Fatalf("chapter 1 should be a title only:\n%s", got)
	}
	if !strings.Contains(got, "brief of Parser") || !strings.HasSuffix(got, "[The request you are working on, verbatim:]\nfix the parser please") {
		t.Fatalf("chapter 2 brief or the request missing:\n%s", got)
	}
}

func TestChapterView_IdenticalAcrossCallsAndPaired(t *testing.T) {
	h := rounds(3, "ok")
	h = append(h, chaptersCompaction("k", "r2u", 0, "", ch(1, "One", "r1u", "r1a")))
	a, b := contextView(h, ""), contextView(h, "")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the view changed between two calls")
	}
	// The cut is at a user message; no tool result opens the unfolded part.
	if a[1].Role != "user" {
		t.Fatalf("the unfolded part should start at a user message: %s", roles(a))
	}
	h2 := rounds(3, "ok")
	h2 = append(h2, chaptersCompaction("k", "r2c", 0, "", ch(1, "One", "r1u", "r2u")))
	v := contextView(h2, "")
	for i, m := range v {
		if m.Role == "tool" && (i == 0 || len(v[i-1].ToolCalls) == 0 && v[i-1].Role != "tool") {
			t.Fatalf("tool result %d without its call: %s", i, roles(v))
		}
	}
	if v[1].ID != "r2c" {
		t.Fatalf("tail should start at the assistant message: %+v", v[1])
	}
}

func TestSegmentChapters_BoundariesAndSizes(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	result := strings.Repeat("x", 2000) // about 500 tokens
	msgs := rounds(40, result)
	budget := 24000 // target 3000 tokens
	segs := cm.segmentChapters(msgs, budget)
	if len(segs) < 3 {
		t.Fatalf("expected several chapters, got %d", len(segs))
	}
	total := 0
	for i, seg := range segs {
		total += len(seg)
		if r := seg[0].Role; r != "user" && r != "assistant" {
			t.Fatalf("segment %d starts with a %s message", i, r)
		}
		if tok := cm.EstimateHistoryTokens(seg); i < len(segs)-1 && (tok < 1500 || tok > 4000) {
			t.Fatalf("segment %d has %d tokens, target 3000", i, tok)
		}
	}
	if total != len(msgs) {
		t.Fatalf("segments hold %d of %d messages", total, len(msgs))
	}

	// One round far past the target stays whole.
	big := round(1, strings.Repeat("y", 200000))
	segs = cm.segmentChapters(big, budget)
	if len(segs) != 2 || len(segs[0]) != 3 {
		t.Fatalf("a huge tool result must stay with its call: %d segments, first has %d", len(segs), len(segs[0]))
	}
	if one := cm.segmentChapters(big[:3], budget); len(one) != 1 {
		t.Fatalf("one huge round is one chapter, got %d", len(one))
	}
}

func TestTailStart_KeepsWholeRoundsAndTheLatest(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := rounds(6, strings.Repeat("x", 400)) // about 100 tokens a result
	fs := foldStateOf(h)
	keep := cm.tailStart(h, fs, 250)
	if r := h[keep].Role; r == "tool" || keep == 0 {
		t.Fatalf("tail starts at %d, a %s message", keep, r)
	}
	if got := cm.EstimateHistoryTokens(h[keep:]); got > 250 {
		t.Fatalf("tail holds %d tokens", got)
	}
	// Even when the latest round alone is over the size, it is kept.
	if keep := cm.tailStart(h, fs, 1); keep != len(h)-1 {
		t.Fatalf("latest round dropped: keep = %d of %d", keep, len(h))
	}
}

// briefProvider answers brief requests (told apart by the system prompt) with
// a title line and a brief, and records how many ran at once.
type briefProvider struct {
	fail          bool
	inflight, max atomic.Int32
	mu            sync.Mutex
	systems       []string
}

func (p *briefProvider) StreamTurn(ctx context.Context, msgs []Message, _ []tool.ToolDefinition, system string) (<-chan ModelStreamChunk, <-chan error) {
	c, e := make(chan ModelStreamChunk, 2), make(chan error, 1)
	defer close(c)
	defer close(e)
	if !strings.Contains(system, "brief for one chapter") {
		c <- ModelStreamChunk{Kind: StreamChunkDelta, Delta: "reply"}
		return c, e
	}
	n := p.inflight.Add(1)
	for old := p.max.Load(); n > old && !p.max.CompareAndSwap(old, n); old = p.max.Load() {
	}
	defer p.inflight.Add(-1)
	time.Sleep(10 * time.Millisecond) // long enough for the requests to overlap
	p.mu.Lock()
	p.systems = append(p.systems, system)
	p.mu.Unlock()
	if p.fail {
		e <- errors.New("429 rate limited")
		return c, e
	}
	first, _, _ := strings.Cut(msgs[0].Content, "\n")
	c <- ModelStreamChunk{Kind: StreamChunkDelta, Delta: "Work on " + strings.TrimPrefix(first, "User: ") + "\nBrief for the segment starting at that question."}
	return c, e
}

func chapterAgent(t *testing.T, p ModelProvider) (*Agent, RoleBinding) {
	t.Helper()
	ag := NewAgent(nil, p, "test", nil, nil, nil, nil, NewContextManager(64000, 100), nil)
	return ag, RoleBinding{Provider: p, ProviderName: "test", Model: "m"}
}

func TestCompactByChapters_RecordsChaptersAndKeepsTail(t *testing.T) {
	p := &briefProvider{}
	ag, bound := chapterAgent(t, p)
	ag.history = rounds(40, strings.Repeat("x", 2000))
	for i := range ag.history {
		ag.history[i].CreatedAt = ag.history[0].CreatedAt
	}
	hist := append([]Message(nil), ag.history...)
	_, done, err := ag.compactByChapters(context.Background(), "t", bound, hist, 100000, 500, 24000, "", false)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	rec := ag.history[len(ag.history)-1]
	c := rec.Compaction
	if rec.Role != "compaction" || c.Kind != CompactionChapters || len(c.Chapters) < 3 || c.KeepFrom == "" || c.Request != "" {
		t.Fatalf("recorded %+v", c)
	}
	if p.max.Load() > 4 || p.max.Load() < 2 {
		t.Fatalf("brief requests in flight at once: %d", p.max.Load())
	}
	if c.Chapters[0].N != 1 || c.Chapters[0].From != "r1u" || !strings.HasPrefix(c.Chapters[0].Title, "Work on question 1") {
		t.Fatalf("first chapter %+v", c.Chapters[0])
	}
	// The chapters and the tail cover the history without a gap or overlap.
	prev := ""
	for _, ch := range c.Chapters {
		if prev != "" {
			if i, _ := indexOfMessage(ag.history, prev); ag.history[i+1].ID != ch.From {
				t.Fatalf("gap before chapter %d", ch.N)
			}
		}
		prev = ch.To
	}
	if i, _ := indexOfMessage(ag.history, prev); ag.history[i+1].ID != c.KeepFrom {
		t.Fatalf("tail does not follow the last chapter")
	}
	if !(c.TokensAfter < c.TokensBefore) {
		t.Fatalf("tokens %d -> %d", c.TokensBefore, c.TokensAfter)
	}
}

func TestCompactByChapters_ManualFoldsEverythingAndKeepsTheRequest(t *testing.T) {
	ag, bound := chapterAgent(t, &briefProvider{})
	hist := rounds(2, "ok")
	ag.history = append([]Message(nil), hist...)
	if _, done, _ := ag.compactByChapters(context.Background(), "t", bound, hist, 1000, 100, 24000, "mind the tests", true); !done {
		t.Fatal("nothing folded")
	}
	c := ag.history[len(ag.history)-1].Compaction
	if c.KeepFrom != "" || c.Request != "question 2" {
		t.Fatalf("KeepFrom %q Request %q", c.KeepFrom, c.Request)
	}
	if v := contextView(ag.history, ""); len(v) != 1 || !strings.HasSuffix(v[0].Content, "verbatim:]\nquestion 2") {
		t.Fatalf("view %+v", v)
	}
}

func TestCompactByChapters_FailingSummarizerGetsFallbackBriefs(t *testing.T) {
	ag, bound := chapterAgent(t, &briefProvider{fail: true})
	hist := rounds(3, "ok")
	ag.history = append([]Message(nil), hist...)
	_, done, err := ag.compactByChapters(context.Background(), "t", bound, hist, 1000, 100, 24000, "", true)
	if err != nil || !done {
		t.Fatalf("a failing summarizer must not fail the compaction: done=%v err=%v", done, err)
	}
	c := ag.history[len(ag.history)-1].Compaction.Chapters[0]
	if c.Title != "question 1" || !strings.Contains(c.Brief, "answer 3") || len(c.Brief) > 900 {
		t.Fatalf("fallback chapter %+v", c)
	}
}

func TestCompactByChapters_NothingToFoldFallsBack(t *testing.T) {
	ag, bound := chapterAgent(t, &briefProvider{})
	hist := rounds(1, "ok")
	ag.history = append([]Message(nil), hist...)
	// The latest round is always kept, so a one-round chat has nothing to fold.
	if _, done, _ := ag.compactByChapters(context.Background(), "t", bound, hist, 1000, 100, 24000, "", false); done {
		t.Fatal("folded the only round")
	}
	if len(ag.history) != len(hist) {
		t.Fatal("recorded something")
	}
}

func TestCompactByChapters_TitlesOnlyWhenBriefsAreTooBig(t *testing.T) {
	cm := NewContextManager(64000, 100)
	big := strings.Repeat("word ", 400) // 500 tokens
	chapters := []Chapter{{Brief: big}, {Brief: big}, {Brief: big}}
	if got := cm.fitTitles(chapters, 0, 1200); got != 1 {
		t.Fatalf("titlesOnly = %d, want 1", got)
	}
	if got := cm.fitTitles(chapters, 2, 1200); got != 2 {
		t.Fatalf("never lowered: %d", got)
	}
	if got := cm.fitTitles(chapters, 0, 100000); got != 0 {
		t.Fatalf("everything fits: %d", got)
	}
}

func TestChaptersStyleOffUsesSummary(t *testing.T) {
	SetCompactDefaults(0, "", "summary")
	defer SetCompactDefaults(0, "", "")
	ag, bound := chapterAgent(t, &briefProvider{})
	ag.history = rounds(3, "ok")
	// "reply" is not a summary request's answer, but it is non-empty: the
	// summary path ran, not the chapters one.
	if _, err := ag.compactConversation(context.Background(), "t", bound, ag.history, ag.history, 1000, 100, 24000, "", true); err != nil {
		t.Fatal(err)
	}
	if k := ag.history[len(ag.history)-1].Compaction.Kind; k != CompactionSummary {
		t.Fatalf("kind %q", k)
	}
}

func recallHistory() []Message {
	h := rounds(6, "ok")
	h[2].Content = "stack trace: nil pointer in parseHeader at lexer.go:88"
	h[6+0].Content = "question about lexer"
	h[10].Content = "config.yaml has port 8080; parseHeader appears again"
	h[14].Content = "unrelated output"
	h = append(h, chaptersCompaction("k", "r5u", 0, "",
		ch(1, "Lexer crash", "r1u", "r2a"),
		ch(2, "Config work", "r3u", "r3a"),
		ch(3, "Misc", "r4u", "r4a")))
	return h
}

func TestRecall_SearchRanksByDistinctTermsThenCountThenNewer(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := recallHistory()
	out, errText := cm.recall(h, json.RawMessage(`{"query":"parseHeader lexer.go"}`), 4000)
	if errText != "" || !strings.HasPrefix(out, "[Chapter 1: Lexer crash — unfolded, messages r1u–r2a]\n") {
		t.Fatalf("%q %q", out, errText)
	}
	// The full messages come back, tool results included.
	if !strings.Contains(out, "stack trace: nil pointer in parseHeader at lexer.go:88") || !strings.Contains(out, "Tool call bash") {
		t.Fatalf("transcript missing:\n%s", out)
	}
	if !strings.Contains(out, "Other matches:\n- Chapter 2: Config work — ") || strings.Contains(out, "Chapter 3") {
		t.Fatalf("other matches wrong:\n%s", out)
	}
	// One term in both chapters: the tie goes to the newer one.
	out, _ = cm.recall(h, json.RawMessage(`{"query":"parseheader"}`), 4000)
	if !strings.HasPrefix(out, "[Chapter 2:") {
		t.Fatalf("a tie should open the newer chapter:\n%s", out)
	}
	// The match is in a message, not the brief: it was searched in full.
	out, _ = cm.recall(h, json.RawMessage(`{"query":"8080"}`), 4000)
	if !strings.HasPrefix(out, "[Chapter 2:") || strings.Contains(out, "Other matches") {
		t.Fatalf("%s", out)
	}
}

func TestRecall_OpenByNumberAndErrors(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := recallHistory()
	out, errText := cm.recall(h, json.RawMessage(`{"chapter":3}`), 4000)
	if errText != "" || !strings.HasPrefix(out, "[Chapter 3: Misc") || !strings.Contains(out, "unrelated output") {
		t.Fatalf("%q %q", out, errText)
	}
	if _, errText = cm.recall(h, json.RawMessage(`{"chapter":4}`), 4000); !strings.Contains(errText, "1 to 3") {
		t.Fatalf("error should name the range: %q", errText)
	}
	if _, errText = cm.recall(h, json.RawMessage(`{}`), 4000); errText == "" {
		t.Fatal("one of query and chapter is required")
	}
	if _, errText = cm.recall(rounds(2, "ok"), json.RawMessage(`{"chapter":1}`), 4000); errText == "" {
		t.Fatal("no chapters, no recall")
	}
}

func TestRecall_NoMatchListsTheChapters(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	out, errText := cm.recall(recallHistory(), json.RawMessage(`{"query":"zebra x"}`), 4000)
	if errText != "" || !strings.HasPrefix(out, `No chapter matches "zebra x".`) || !strings.Contains(out, "- 1: Lexer crash\n- 2: Config work\n- 3: Misc") {
		t.Fatalf("%q %q", out, errText)
	}
}

func TestRecall_CapShrinksToolResults(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := rounds(3, strings.Repeat("line of output\n", 4000)) // 15000 tokens a result
	h = append(h, chaptersCompaction("k", "", 0, "", ch(1, "Big", "r1u", "r3a")))
	out, _ := cm.recall(h, json.RawMessage(`{"chapter":1}`), 4000)
	if got := cm.EstimateTokens(out); got > 4000+200 {
		t.Fatalf("result holds %d tokens, cap 4000", got)
	}
	if !strings.Contains(out, "Long tool results were shortened") || !strings.Contains(out, "characters cut") || !strings.Contains(out, "question 3") {
		t.Fatalf("expected shortened results with every message kept:\n%.600s", out)
	}
	// Under the cap nothing is touched.
	small := cm.openChapter(ch(1, "t", "a", "b"), []Message{{Role: "user", Content: "hi"}}, 4000)
	if strings.Contains(small, "shortened") {
		t.Fatal("note on a result that fit")
	}
}

func TestRecall_AlreadyOpenAndRecallResultsAreNotSearched(t *testing.T) {
	cm := NewContextManager(64000, 4000)
	h := recallHistory()
	// The model recalled chapter 1 after the compaction; the result is in the view.
	h = append(h,
		Message{ID: "q1", Role: "assistant", ToolCalls: []ToolCall{{ID: "rc1", Name: RecallToolName, Args: json.RawMessage(`{"query":"zebra"}`)}}},
		Message{ID: "q2", Role: "tool", ToolCallID: "rc1", Content: "[Chapter 1: Lexer crash — unfolded, messages r1u–r2a]\nzebra zebra zebra"})
	out, _ := cm.recall(h, json.RawMessage(`{"chapter":1}`), 4000)
	if out != "Chapter 1 is already open above." {
		t.Fatalf("%q", out)
	}
	// Neither the call's query nor the unfolded copy's text make a match.
	if out, _ = cm.recall(h, json.RawMessage(`{"query":"zebra"}`), 4000); !strings.HasPrefix(out, "No chapter matches") {
		t.Fatalf("recall exchange was searched:\n%s", out)
	}
	// Once a compaction folds the copy away, the chapter can be opened again.
	h = append(h, chaptersCompaction("k2", "", 0, "", ch(4, "Recalled", "q1", "q2")))
	if out, _ = cm.recall(h, json.RawMessage(`{"chapter":1}`), 4000); !strings.HasPrefix(out, "[Chapter 1:") {
		t.Fatalf("%q", out)
	}
	// And it is also not open when a clear compaction stubbed it.
	h2 := append(recallHistory()[:len(recallHistory()):len(recallHistory())],
		Message{ID: "q1", Role: "assistant", ToolCalls: []ToolCall{{ID: "rc1", Name: RecallToolName, Args: json.RawMessage(`{"chapter":1}`)}}},
		Message{ID: "q2", Role: "tool", ToolCallID: "rc1", Content: "[Chapter 1: Lexer crash — unfolded, messages r1u–r2a]\n..."},
		Message{ID: "q3", Role: "user", Content: "go on"},
		Message{ID: "cl", Role: "compaction", Compaction: &Compaction{Kind: CompactionClear, ClearBefore: "q3"}})
	if out, _ = cm.recall(h2, json.RawMessage(`{"chapter":1}`), 4000); !strings.HasPrefix(out, "[Chapter 1:") {
		t.Fatalf("a cleared copy is not open: %q", out)
	}
}

// scriptProvider answers ordinary requests from steps, one per request, and
// notes the tools each was offered.
type scriptProvider struct {
	mu    sync.Mutex
	steps [][]ModelStreamChunk
	tools [][]string
}

func (p *scriptProvider) StreamTurn(_ context.Context, _ []Message, defs []tool.ToolDefinition, _ string) (<-chan ModelStreamChunk, <-chan error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	p.tools = append(p.tools, names)
	step := []ModelStreamChunk{{Kind: StreamChunkDelta, Delta: "done"}}
	if len(p.steps) > 0 {
		step, p.steps = p.steps[0], p.steps[1:]
	}
	c, e := make(chan ModelStreamChunk, len(step)), make(chan error, 1)
	for _, ch := range step {
		c <- ch
	}
	close(c)
	close(e)
	return c, e
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// recall is offered only once chapters exist, in every mode, and its result
// reaches the model whole although TruncateToolOutput would have cut it.
func TestRecall_OfferedOnlyWithChaptersAndNotTruncated(t *testing.T) {
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := NewDiskSessionStore(dir)
	p := &scriptProvider{}
	ag := NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), &lockedBuf{}), p, "test", nil, permission.New(norm, nil), snapStore, sessStore, NewContextManager(64000, 100), nil)
	t.Cleanup(ag.WaitBackground)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "hello"); err != nil {
		t.Fatal(err)
	}
	if hasTool(p.tools[0], RecallToolName) {
		t.Fatalf("recall offered with no chapters: %v", p.tools[0])
	}

	long := strings.Repeat("a long line of output\n", 400) // far past 100 tokens
	ag.mu.Lock()
	ag.history = rounds(2, long)
	ag.history = append(ag.history, chaptersCompaction("k", "", 0, "", ch(1, "Output", "r1u", "r2a")))
	ag.mu.Unlock()
	for _, mode := range []Mode{ModeReadWrite, ModeReadOnly, ModePlan} {
		ag.SetMode(mode)
		p.steps = [][]ModelStreamChunk{{{Kind: StreamChunkToolCall, ToolCall: &ToolCall{ID: "rc" + string(mode), Name: RecallToolName, Args: json.RawMessage(`{"chapter":1}`)}}}}
		n := len(p.tools)
		if err := ag.RunTurn(context.Background(), "s1", "t"+string(mode), "what was in chapter 1?"); err != nil {
			t.Fatal(err)
		}
		if !hasTool(p.tools[n], RecallToolName) {
			t.Fatalf("%s: recall not offered with chapters: %v", mode, p.tools[n])
		}
		ag.mu.Lock()
		var res string
		for _, m := range ag.history {
			if m.Role == "tool" && m.ToolCallID == "rc"+string(mode) {
				res = m.Content
			}
		}
		ag.mu.Unlock()
		if !strings.HasPrefix(res, "[Chapter 1: Output") || strings.Contains(res, "were cut to save context") || strings.Count(res, "a long line of output") < 800 {
			t.Fatalf("%s: recall result cut or missing: %.200s", mode, res)
		}
		// Fold the opened copy away so the next mode can open it again.
		ag.mu.Lock()
		ag.history = append(ag.history, chaptersCompaction("kk"+string(mode), "", 1, "", ch(len(foldStateOf(ag.history).chapters)+1, "Recalled", "r1u", "r1u")))
		ag.mu.Unlock()
	}
}

// lockedBuf is an io.Writer safe against background work writing events.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func TestBreakdown_ChapterIndexCountsAsSummary(t *testing.T) {
	cm := NewContextManager(8000, 4000)
	h := append(rounds(1, "ok"), chaptersCompaction("k", "", 0, "", Chapter{N: 1, Title: "T", Brief: strings.Repeat("brief ", 300), From: "r1u", To: "r1a"}))
	h = append(h, Message{ID: "n", Role: "user", Content: "next"})
	by := catMap(cm.Breakdown(h, "", "sys", oneTool, false).Categories)
	if by[protocol.CtxSummary].Tokens < 400 || by[protocol.CtxUser].Tokens > 20 {
		t.Fatalf("%+v", by)
	}
}
