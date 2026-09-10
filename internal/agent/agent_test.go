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
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

type ScriptedProvider struct {
	turns [][]agent.ModelStreamChunk
	index int
}

func (s *ScriptedProvider) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 10)
	eChan := make(chan error, 1)

	if s.index < len(s.turns) {
		turn := s.turns[s.index]
		s.index++
		go func() {
			for _, item := range turn {
				cChan <- item
			}
			close(cChan)
			close(eChan)
		}()
	} else {
		close(cChan)
		close(eChan)
	}
	return cChan, eChan
}

func TestAgent_MultiTurnSessionPersistenceAndContextManagement(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	sb := sandbox.NewSandbox(tempDir, 2*time.Second)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
	}

	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{Kind: agent.StreamChunkDelta, Delta: "Turn 1 complete"},
			},
			{
				{Kind: agent.StreamChunkDelta, Delta: "Turn 2 complete"},
			},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, tools, engine, snapStore, sessStore, ctxMgr)

	ctx := context.Background()

	if err := ag.RunTurn(ctx, "session-alpha", "turn-1", "Hello 1"); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	if err := ag.RunTurn(ctx, "session-alpha", "turn-2", "Hello 2"); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	savedSession, err := sessStore.Load("session-alpha")
	if err != nil {
		t.Fatalf("failed loading saved session: %v", err)
	}
	if len(savedSession.History) != 4 {
		t.Fatalf("expected 4 messages in history, got %d", len(savedSession.History))
	}
}
