package agent

import (
	"strings"
	"testing"

	"yoakeland/internal/skills"
	"yoakeland/internal/tool"
)

func testSkill(name string) skills.Skill {
	return skills.Skill{Name: name, Agents: []string{skills.Native}, Enabled: true, UserInvocable: true}
}

func readBody(name string) string { return "BODY(" + name + ")" }

func skillCall(id, name string) ToolCall {
	return ToolCall{ID: id, Name: tool.SkillToolName, Args: []byte(`{"name":"` + name + `"}`)}
}

// A skill named in several messages is put in the request once, on the first.
func TestWithNamedSkills_FirstMentionCarriesIt(t *testing.T) {
	have := []skills.Skill{testSkill("review")}
	view := []Message{
		{Role: "user", Content: "use $review here"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "and $review again"},
	}
	out := withNamedSkills(view, have, nil, readBody)
	if !strings.Contains(out[0].Content, "BODY(review)") {
		t.Fatalf("first mention lacks the skill: %q", out[0].Content)
	}
	if strings.Contains(out[2].Content, "BODY(review)") {
		t.Fatalf("second mention repeats the skill: %q", out[2].Content)
	}
	if view[0].Content != "use $review here" {
		t.Fatal("the history's own message was changed")
	}
}

// A skill the model loaded itself isn't repeated on a later $name, but a $name
// that came before the model's own load still carries it.
func TestWithNamedSkills_SkipsWhatTheModelLoaded(t *testing.T) {
	have := []skills.Skill{testSkill("review")}
	view := []Message{
		{Role: "user", Content: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{skillCall("c1", "review")}},
		{Role: "tool", ToolCallID: "c1", Content: "<skill>…</skill>"},
		{Role: "user", Content: "now $review"},
	}
	if out := withNamedSkills(view, have, nil, readBody); strings.Contains(out[3].Content, "BODY") {
		t.Fatalf("repeated a skill already in the view: %q", out[3].Content)
	}
	early := []Message{{Role: "user", Content: "do $review"}, view[1], view[2]}
	if out := withNamedSkills(early, have, nil, readBody); !strings.Contains(out[0].Content, "BODY(review)") {
		t.Fatal("a mention before the model's own load must carry the skill")
	}
}

// Compaction folds the conversation into a summary; the skills it had brought in
// ride on the summary so their instructions aren't lost.
func TestSkills_SurviveASummaryCompaction(t *testing.T) {
	have := []skills.Skill{testSkill("review"), testSkill("polish")}
	history := []Message{
		{ID: "u1", Role: "user", Content: "please $review"},
		{ID: "a1", Role: "assistant", ToolCalls: []ToolCall{skillCall("c1", "polish")}},
		{ID: "t1", Role: "tool", ToolCallID: "c1", Content: "polish text"},
		{ID: "c", Role: "compaction", Content: "we reviewed", Compaction: &Compaction{Kind: CompactionSummary}},
		{ID: "u2", Role: "user", Content: "carry on with $review"},
	}
	carried := carriedSkills(history, have)
	if strings.Join(carried, ",") != "review,polish" {
		t.Fatalf("carried = %v", carried)
	}
	out := withNamedSkills(contextView(history, ""), have, carried, readBody)
	if !strings.HasPrefix(out[0].Content, summaryOpening) || !strings.Contains(out[0].Content, "BODY(review)") || !strings.Contains(out[0].Content, "BODY(polish)") {
		t.Fatalf("the summary lost the skills: %q", out[0].Content)
	}
	if strings.Contains(out[1].Content, "BODY") {
		t.Fatalf("the later mention repeats a carried skill: %q", out[1].Content)
	}
}

// A clearing compaction stubs old tool results, but not a skill's instructions.
func TestContextView_ClearLeavesSkillResults(t *testing.T) {
	history := []Message{
		{ID: "a1", Role: "assistant", ToolCalls: []ToolCall{skillCall("c1", "review"), {ID: "c2", Name: "file_read", Args: []byte(`{}`)}}},
		{ID: "t1", Role: "tool", ToolCallID: "c1", Content: "skill text\nline"},
		{ID: "t2", Role: "tool", ToolCallID: "c2", Content: "file text\nline"},
		{ID: "u2", Role: "user", Content: "next"},
		{ID: "c", Role: "compaction", Compaction: &Compaction{Kind: CompactionClear, ClearBefore: "u2"}},
	}
	got := map[string]string{}
	for _, m := range contextView(history, "") {
		if m.Role == "tool" {
			got[m.ToolCallID] = m.Content
		}
	}
	if got["c1"] != "skill text\nline" {
		t.Fatalf("skill result was cleared: %q", got["c1"])
	}
	if !strings.HasPrefix(got["c2"], "[cleared") {
		t.Fatalf("ordinary result was kept: %q", got["c2"])
	}
}
