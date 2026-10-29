package agent_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
)

// The Stop button cancels the turn, not a subagent: with one still working the turn must end anyway.
func TestSubagents_CancellingTheTurnEndsItWhileOneWorks(t *testing.T) {
	norm, _ := normalizer.New(t.TempDir())
	snapStore, _ := snapshot.NewDiskSnapshotStore(t.TempDir())
	sessStore, _ := agent.NewDiskSessionStore(t.TempDir())
	sink := &lineSink{}
	ag := agent.NewAgent(protocol.NewIPCStream(bytes.NewBuffer(nil), sink), stuckSubagentProvider{}, "p", nil, permission.New(norm, nil), snapStore, sessStore, agent.NewContextManager(100000, 4000), nil)
	ag.SetMode(agent.ModeAutonomous)
	t.Cleanup(ag.WaitBackground)

	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			for _, env := range sink.envelopes() {
				if env.Type == protocol.EventSubagentStatus && env.ParentToolCallID == "sub-x" {
					ag.CancelTurn("t1")
					return
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- ag.RunTurn(context.Background(), "s", "t1", "go") }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did nothing: the turn kept waiting on its subagent")
	}
}
