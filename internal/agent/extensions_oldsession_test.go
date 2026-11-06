package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/skills"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// A pre-2.36 session keeps the skill load a turn fabricated (assistant call +
// tool result, IDs prefixed skill_<turnId>_<letter>, ours by construction). The
// request view folds it away: the pair is dropped, and the message that named
// the skill carries the instructions again.
func TestSkills_OldSessionPreloadPairIsFoldedAway(t *testing.T) {
	fs := &fakeSkills{list: []skills.Skill{reviewSkill()}, docs: map[string]tool.SkillDoc{"review": {Name: "review", Dir: "/skills/review", Body: "Check every changed file."}}}
	model := &scriptedModel{turns: [][]agent.ModelStreamChunk{say("done")}}

	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	engine := permission.New(norm, nil)
	snap, _ := snapshot.NewDiskSnapshotStore(dir)
	sess, _ := agent.NewDiskSessionStore(dir)
	argsd, _ := json.Marshal(map[string]string{"name": "review"})
	if err := sess.Save(&agent.SessionState{
		SessionID: "s-old", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		History: []agent.Message{
			{ID: "u1", Role: "user", Content: "please $review the old diff"},
			{ID: "a1", Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "skill_t-old_a", Name: "skill", Args: argsd}}},
			{ID: "t1", Role: "tool", ToolCallID: "skill_t-old_a", Content: "<skill name=\"review\">\nOLD text\n</skill>"},
			{ID: "a2", Role: "assistant", Content: "did"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	ag := agent.NewAgent(ipc, model, "test", nil, engine, snap, sess, agent.NewContextManager(100000, 4000), nil)
	ag.SetSkills(fs)
	t.Cleanup(ag.WaitBackground)
	if err := ag.LoadSession("s-old"); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunTurn(context.Background(), "s-old", "t2", "go"); err != nil {
		t.Fatal(err)
	}
	t.Logf("events: %s", out.String())
	var roles []string
	for _, m := range model.history[0] {
		roles = append(roles, m.Role)
		if m.Role == "tool" {
			t.Fatalf("the fabricated tool result reached the model: %+v", m)
		}
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("the fabricated assistant call should be folded away: %v", roles)
	}
	got := model.history[0][0].Content
	if !strings.Contains(got, "Check every changed file.") || strings.Contains(got, "OLD text") {
		t.Fatalf("the user message should now carry the skill's own body: %q", got)
	}
}
