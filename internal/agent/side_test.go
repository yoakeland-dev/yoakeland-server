package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// sideProvider answers side questions and summaries (told apart by system prompt) and
// remembers the user message of each, so a test can see what the model was shown.
type sideProvider struct {
	mu      sync.Mutex
	prompts []string
}

func (p *sideProvider) StreamTurn(_ context.Context, h []agent.Message, _ []tool.ToolDefinition, system string) (<-chan agent.ModelStreamChunk, <-chan error) {
	text := "reply"
	switch {
	case strings.Contains(system, "side question"):
		text = "side answer"
	case strings.Contains(system, "Recap it for the user in the first person"):
		text = "short summary"
	}
	if text != "reply" {
		p.mu.Lock()
		p.prompts = append(p.prompts, h[len(h)-1].Content)
		p.mu.Unlock()
	}
	c, e := make(chan agent.ModelStreamChunk, 1), make(chan error, 1)
	c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: text}
	close(c)
	close(e)
	return c, e
}

func ask(ag *agent.Agent, anchor, q string) (last protocol.SideAnswerData) {
	ag.SideQuestion(context.Background(), "s1", "r1", anchor, q, func(d protocol.SideAnswerData) { last = d })
	return last
}

func TestSideQuestion_BtwLeavesNothingBehind(t *testing.T) {
	p := &sideProvider{}
	ag, out := compactFixture(t, p)
	before := out.String()
	if d := ask(ag, "", "why?"); !d.Done || d.Text != "side answer" || d.Error != "" {
		t.Fatalf("answer = %+v", d)
	}
	if out.String() != before {
		t.Fatalf("a /btw wrote to the session's stream: %s", strings.TrimPrefix(out.String(), before))
	}
	// The next thread question sees no trace of it.
	ask(ag, "m2", "again?")
	if strings.Contains(p.prompts[1], "why?") {
		t.Fatalf("/btw leaked into a later prompt: %s", p.prompts[1])
	}
}

func TestSideQuestion_ThreadKeepsItsTurnsAndFeedsThemBack(t *testing.T) {
	p := &sideProvider{}
	ag, _ := compactFixture(t, p)
	ask(ag, "m2", "first?")
	ask(ag, "m2", "second?")
	if got := p.prompts[1]; !strings.Contains(got, "User: first?") || !strings.Contains(got, "Assistant: side answer") || !strings.Contains(got, "Side question: second?") {
		t.Fatalf("second prompt lacks the thread: %s", got)
	}
	if d := ask(ag, "nope", "x"); d.Error == "" {
		t.Fatal("a thread on a missing message should fail")
	}
}

func TestSummarizeMessage(t *testing.T) {
	ag, _ := compactFixture(t, &sideProvider{})
	if s, _, _, err := ag.SummarizeMessage(context.Background(), "m2"); err != nil || s != "short summary" {
		t.Fatalf("got %q, %v", s, err)
	}
	if _, _, _, err := ag.SummarizeMessage(context.Background(), "m1"); err == nil {
		t.Fatal("a user message shouldn't be summarized")
	}
}
