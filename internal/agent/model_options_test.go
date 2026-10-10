package agent_test

import (
	"bytes"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
)

// TunableProvider records whatever SetModelOptions last applied, standing in
// for the real AnthropicProvider/OpenAICompatibleProvider in this package's
// tests (they live in internal/provider, which would import internal/agent
// back — a cycle).
type TunableProvider struct {
	ScriptedProvider
	lastOptions agent.ModelOptions
}

func (t *TunableProvider) SetModelOptions(opts agent.ModelOptions) {
	t.lastOptions = opts
}

func TestAgent_SetModelOptions_AppliesToProviderAndContextWindow(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	tunable := &TunableProvider{ScriptedProvider: ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "ok"}}},
	}}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, tunable, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)

	applied := ag.SetModelOptions(agent.ModelOptions{ReasoningEffort: "high", ThinkingBudget: 4096}, 8000, nil, nil)
	if !applied {
		t.Fatal("expected SetModelOptions to report the provider accepted the options")
	}
	if tunable.lastOptions.ReasoningEffort != "high" || tunable.lastOptions.ThinkingBudget != 4096 {
		t.Errorf("provider did not receive the options: %+v", tunable.lastOptions)
	}
	if ctxMgr.MaxHistoryTokens != 8000 {
		t.Errorf("expected ContextWindow to resize the context manager, got MaxHistoryTokens=%d", ctxMgr.MaxHistoryTokens)
	}
}

// TestAgent_SetModelOptions_IgnoredByPlainProvider guards the CLI-subprocess
// case: a provider that doesn't implement ModelOptionsSetter must not panic
// and should report that the options were not applied, so a caller can tell
// a client the reasoning/thinking fields were silently ignored.
func TestAgent_SetModelOptions_IgnoredByPlainProvider(t *testing.T) {
	tempDir := t.TempDir()
	norm, _ := normalizer.New(tempDir)
	engine := permission.New(norm, nil)
	snapStore, _ := snapshot.NewDiskSnapshotStore(tempDir)
	sessStore, _ := agent.NewDiskSessionStore(tempDir)
	ctxMgr := agent.NewContextManager(100, 20)

	plain := &ScriptedProvider{}
	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, plain, "test-provider", nil, engine, snapStore, sessStore, ctxMgr, nil)
	t.Cleanup(ag.WaitBackground)

	applied := ag.SetModelOptions(agent.ModelOptions{ReasoningEffort: "high"}, 0, nil, nil)
	if applied {
		t.Error("expected SetModelOptions to report false for a provider without ModelOptionsSetter")
	}
}

var _ agent.ModelProvider = (*TunableProvider)(nil)
