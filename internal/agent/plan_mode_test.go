package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// capturingProvider records the tool definitions it was handed on each
// StreamTurn call and plays back a scripted turn per call.
type capturingProvider struct {
	toolNamesByTurn [][]string
	turns           [][]agent.ModelStreamChunk
	index           int
}

func (c *capturingProvider) StreamTurn(_ context.Context, _ []agent.Message, tools []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	var names []string
	for _, td := range tools {
		names = append(names, td.Name)
	}
	c.toolNamesByTurn = append(c.toolNamesByTurn, names)

	cChan := make(chan agent.ModelStreamChunk, 10)
	eChan := make(chan error, 1)
	if c.index < len(c.turns) {
		turn := c.turns[c.index]
		c.index++
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

// TestAgent_PlanModeRestrictsToolsGivenToModel guards the primary safety
// mechanism: while plan mode is on, the model isn't handed the write tool's
// definition at all, so it has nothing to call — not just something that
// gets denied after the fact.
func TestAgent_PlanModeRestrictsToolsGivenToModel(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
	}

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "normal turn"}},
			{{Kind: agent.StreamChunkDelta, Delta: "plan turn"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "hi"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(provider.toolNamesByTurn[0]) != 2 {
		t.Fatalf("expected both tools in normal mode, got %v", provider.toolNamesByTurn[0])
	}

	ag.SetPlanMode(true)
	if !ag.PlanMode() {
		t.Fatal("expected PlanMode() to report true after SetPlanMode(true)")
	}
	if err := ag.RunTurn(context.Background(), "s1", "t2", "plan this"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	got := provider.toolNamesByTurn[1]
	if len(got) != 1 || got[0] != "file_read" {
		t.Fatalf("expected only file_read in plan mode, got %v", got)
	}
}

// TestAgent_PlanModeBlocksMutatingToolCall guards the defense-in-depth
// backstop: even if a tool call for a mutating tool somehow arrives while in
// plan mode, it's rejected rather than executed.
func TestAgent_PlanModeBlocksMutatingToolCall(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
	}

	writeArgs, _ := json.Marshal(map[string]string{"path": "should-not-exist.txt", "content": "x"})

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{
				{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc1", Name: "file_write", Args: writeArgs}},
			},
			{{Kind: agent.StreamChunkDelta, Delta: "done"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)
	ag.SetPlanMode(true)

	if err := ag.RunTurn(context.Background(), "s2", "t1", "try to write"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempDir, "should-not-exist.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected file_write to be blocked in plan mode, but the file exists")
	}
	if !strings.Contains(outBuf.String(), "unavailable while in plan mode") {
		t.Fatalf("expected tool_result to report the plan-mode rejection, got: %s", outBuf.String())
	}
}
