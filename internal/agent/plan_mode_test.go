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

func (c *capturingProvider) StreamTurn(_ context.Context, _ []agent.Message, tools []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
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

// TestAgent_ModeRestrictsToolsGivenToModel guards the primary safety
// mechanism for each of the four tiers: the model isn't handed a
// restricted tool's definition at all, so it has nothing to call — not
// just something that gets denied after the fact.
func TestAgent_ModeRestrictsToolsGivenToModel(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewWritePlanTool(norm),
	}

	provider := &capturingProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "read/write turn"}},
			{{Kind: agent.StreamChunkDelta, Delta: "read-only turn"}},
			{{Kind: agent.StreamChunkDelta, Delta: "plan turn"}},
			{{Kind: agent.StreamChunkDelta, Delta: "autonomous turn"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", tools, engine, snapStore, sessStore, ctxMgr, nil)

	if err := ag.RunTurn(context.Background(), "s1", "t1", "hi"); err != nil {
		t.Fatalf("turn 1 (read/write): %v", err)
	}
	if got := provider.toolNamesByTurn[0]; len(got) != 3 {
		t.Fatalf("expected all 3 tools in read/write mode, got %v", got)
	}

	ag.SetMode(agent.ModeReadOnly)
	if ag.Mode() != agent.ModeReadOnly {
		t.Fatal("expected Mode() to report ModeReadOnly")
	}
	if err := ag.RunTurn(context.Background(), "s1", "t2", "read only"); err != nil {
		t.Fatalf("turn 2 (read-only): %v", err)
	}
	if got := provider.toolNamesByTurn[1]; len(got) != 1 || got[0] != "file_read" {
		t.Fatalf("expected only file_read in read-only mode, got %v", got)
	}

	ag.SetMode(agent.ModePlan)
	if err := ag.RunTurn(context.Background(), "s1", "t3", "plan this"); err != nil {
		t.Fatalf("turn 3 (plan): %v", err)
	}
	got := provider.toolNamesByTurn[2]
	if len(got) != 2 {
		t.Fatalf("expected file_read + write_plan in plan mode, got %v", got)
	}
	for _, want := range []string{"file_read", "write_plan"} {
		found := false
		for _, name := range got {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %q among plan mode tools, got %v", want, got)
		}
	}

	ag.SetMode(agent.ModeAutonomous)
	if err := ag.RunTurn(context.Background(), "s1", "t4", "go"); err != nil {
		t.Fatalf("turn 4 (autonomous): %v", err)
	}
	if got := provider.toolNamesByTurn[3]; len(got) != 3 {
		t.Fatalf("expected all 3 tools in autonomous mode, got %v", got)
	}
}

// TestAgent_ReadOnlyModeBlocksMutatingToolCall guards the defense-in-depth
// backstop: even if a tool call for a mutating tool somehow arrives while in
// a restricted mode, it's rejected rather than executed.
func TestAgent_ReadOnlyModeBlocksMutatingToolCall(t *testing.T) {
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
	ag.SetMode(agent.ModeReadOnly)

	if err := ag.RunTurn(context.Background(), "s2", "t1", "try to write"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempDir, "should-not-exist.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected file_write to be blocked in read-only mode, but the file exists")
	}
	if !strings.Contains(outBuf.String(), "unavailable in the current mode") {
		t.Fatalf("expected tool_result to report the mode rejection, got: %s", outBuf.String())
	}
}
