package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/tool"
)

// ScriptedProvider replays pre-programmed turns for mock testing
type ScriptedProvider struct {
	turns [][]agent.ModelStreamChunk
	index int
}

func (s *ScriptedProvider) StreamTurn(_ context.Context, _ []agent.Message) (<-chan agent.ModelStreamChunk, <-chan error) {
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

func TestAgentLoop_WithPermissionAndToolDispatch(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox.NewLocalSandbox(tempDir, 2*time.Second)
	engine := permission.New(norm)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewBashTool(norm, sb),
	}

	// Turn 1: Model requests bash tool: `echo "agent ok"`
	// Turn 2: Model finishes turn with delta
	scripted := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{
					Kind: agent.StreamChunkToolCall,
					ToolCall: &agent.ToolCall{
						ID:   "call-1",
						Name: "bash",
						Args: json.RawMessage(`{"command":"echo 'agent ok'"}`),
					},
				},
			},
			{
				{Kind: agent.StreamChunkDelta, Delta: "I finished the task."},
			},
		},
	}

	outBuf := &bytes.Buffer{}
	ipcStream := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipcStream, scripted, tools, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Intercept permission requirement concurrently
	go func() {
		for {
			time.Sleep(20 * time.Millisecond)
			// Automatically grant permission in test
			ag.HandlePermissionResponse("call-1", true, true, "bash:echo 'agent ok'")
		}
	}()

	err = ag.RunTurn(ctx, "turn-1", "Execute echo")
	if err != nil {
		t.Fatalf("agent loop failed: %v", err)
	}

	// Verify expected events in output stream
	output := outBuf.String()
	if !bytes.Contains(outBuf.Bytes(), []byte(protocol.EventToolCallRequested)) {
		t.Errorf("missing tool_call_requested event: %s", output)
	}
	if !bytes.Contains(outBuf.Bytes(), []byte(protocol.EventTurnCompleted)) {
		t.Errorf("missing turn_completed event: %s", output)
	}
}
