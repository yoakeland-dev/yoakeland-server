package agent_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// summaryProvider answers ordinary turns with "reply" and each summarizer
// request (told apart by its system prompt) with the next of summaries,
// remembering the prompts it was given.
type summaryProvider struct {
	mu        sync.Mutex
	summaries []string
	systems   []string
}

func (p *summaryProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	text := "reply"
	if strings.Contains(system, "summarize a coding agent's session") {
		p.mu.Lock()
		p.systems = append(p.systems, system)
		text, p.summaries = p.summaries[0], p.summaries[1:]
		p.mu.Unlock()
	}
	c, e := make(chan agent.ModelStreamChunk, 2), make(chan error, 1)
	c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text}
	close(c)
	close(e)
	return c, e
}

func compactFixture(t *testing.T, p agent.ModelProvider) (*agent.Agent, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	out := &bytes.Buffer{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), p, "test", nil, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(64000, 100), nil)
	t.Cleanup(ag.WaitBackground)
	t.Cleanup(func() { agent.SetCompactDefaults(0, "") })
	if err := ag.RunTurn(context.Background(), "s1", "t1", "hello"); err != nil {
		t.Fatal(err)
	}
	return ag, out
}

func TestCompact_RecordsASummaryAndAnnouncesIt(t *testing.T) {
	long := strings.Repeat("kept detail ", 100)
	ag, out := compactFixture(t, &summaryProvider{summaries: []string{long}})
	if err := ag.Compact(context.Background(), "s1", "t2", ""); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, `"type":"context_compacted"`) || !strings.Contains(s, `"manual":true`) || !strings.Contains(s, "kept detail") {
		t.Fatalf("no manual context_compacted with the summary: %s", s)
	}
}

// A careless instruction ("just say ok") must not leave the agent with a
// one-word memory: the short result is discarded and the base prompt rerun.
func TestCompact_TooShortSummaryWithInstructionsIsRedoneWithout(t *testing.T) {
	long := strings.Repeat("kept detail ", 100)
	p := &summaryProvider{summaries: []string{"ok.", long}}
	ag, out := compactFixture(t, p)
	agent.SetCompactDefaults(0, "just say ok")
	if err := ag.Compact(context.Background(), "s1", "t2", "be brief"); err != nil {
		t.Fatal(err)
	}
	if len(p.systems) != 2 {
		t.Fatalf("expected a second, instruction-free summary request, got %d", len(p.systems))
	}
	if !strings.Contains(p.systems[0], "just say ok") || !strings.Contains(p.systems[0], "be brief") || !strings.Contains(p.systems[0], "does not replace") {
		t.Fatalf("instructions should be appended to the base prompt: %q", p.systems[0])
	}
	if strings.Contains(p.systems[1], "just say ok") {
		t.Fatalf("the retry must run the base prompt alone: %q", p.systems[1])
	}
	if !strings.Contains(out.String(), "kept detail") {
		t.Fatalf("the full summary should be the one recorded: %s", out.String())
	}
}

// summarizerDownProvider answers turns but fails every summarizer request, as
// a rate-limited or unreachable utility model does.
type summarizerDownProvider struct{}

func (summarizerDownProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk, 1), make(chan error, 1)
	if strings.Contains(system, "summarize a coding agent's session") {
		e <- errors.New("429 rate limited")
	} else {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "reply"}
	}
	close(c)
	close(e)
	return c, e
}

// A failed summary must not stand in for the conversation: nothing is
// recorded, so the model keeps the full history.
func TestCompact_FailedSummaryRecordsNothing(t *testing.T) {
	ag, out := compactFixture(t, summarizerDownProvider{})
	if err := ag.Compact(context.Background(), "s1", "t2", ""); err == nil {
		t.Fatal("expected the failed compaction to be reported")
	}
	if s := out.String(); strings.Contains(s, `"type":"context_compacted"`) {
		t.Fatalf("a failed compaction must not be announced: %s", s)
	}
}
