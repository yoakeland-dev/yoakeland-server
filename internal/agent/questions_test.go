package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

func newTestAgent(t *testing.T, dir string, provider agent.ModelProvider, tools []tool.Tool) (*agent.Agent, *syncBuf) {
	t.Helper()
	norm, _ := normalizer.New(dir)
	snapStore, _ := snapshot.NewDiskSnapshotStore(dir)
	sessStore, _ := agent.NewDiskSessionStore(dir)
	out := &syncBuf{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), out), provider, "test-provider", tools,
		permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100, 20), nil)
	t.Cleanup(ag.WaitBackground)
	return ag, out
}

// All of a native agent's questions are pending together, so a client can
// stack them, and the answers come back in question order.
func TestAgent_QuestionsArePendingTogether(t *testing.T) {
	ag, _ := newTestAgent(t, t.TempDir(), &capturingProvider{}, nil)
	qs := []agent.NativeQuestion{{ToolCallID: "q#0", Question: "one"}, {ToolCallID: "q#1", Question: "two"}, {ToolCallID: "q#2", Question: "three"}}
	type result struct {
		got []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := ag.AskQuestions(context.Background(), "s1", "t1", qs)
		done <- result{got, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(ag.PendingAsks()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 3 questions pending", len(ag.PendingAsks()))
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, id := range []string{"q#2", "q#0", "q#1"} { // out of order on purpose
		ag.HandleAskUserAnswer(id, "answer "+id)
	}
	r := <-done
	if r.err != nil || strings.Join(r.got, "|") != "answer q#0|answer q#1|answer q#2" {
		t.Fatalf("answers = %v, %v", r.got, r.err)
	}
}
