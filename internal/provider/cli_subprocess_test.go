package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/provider"
)

func drainStreamTurn(t *testing.T, p *provider.CLISubprocessProvider) ([]agent.ModelStreamChunk, error) {
	t.Helper()
	chunks, errs := p.StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil)

	var got []agent.ModelStreamChunk
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
			} else {
				got = append(got, c)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil {
				return got, err
			}
		}
		if chunks == nil && errs == nil {
			return got, nil
		}
	}
}

// TestCLISubprocessProvider_ClaudeEndToEnd guards the full path from a fake
// `claude` invocation's stdout through to the ModelStreamChunk the rest of
// the Agent loop consumes, using an injected runner so this doesn't depend
// on the real CLI being installed.
func TestCLISubprocessProvider_ClaudeEndToEnd(t *testing.T) {
	p := &provider.CLISubprocessProvider{
		Kind:  provider.CLIKindClaude,
		Model: "claude-3-5-sonnet-20241022",
		Run: func(_ context.Context, name string, args []string, dir string) ([]byte, error) {
			if name != "claude" {
				t.Fatalf("expected to invoke the claude binary, got %q", name)
			}
			return []byte(`{"result":"hi there","is_error":false,"total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":5}}`), nil
		},
	}

	chunks, err := drainStreamTurn(t, p)
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected a delta chunk and a usage chunk, got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Kind != agent.StreamChunkDelta || chunks[0].Delta != "hi there" {
		t.Fatalf("unexpected first chunk: %+v", chunks[0])
	}
	if chunks[1].Kind != agent.StreamChunkUsage || chunks[1].Usage.CostUSD != 0.02 {
		t.Fatalf("unexpected second chunk: %+v", chunks[1])
	}
}

func TestCLISubprocessProvider_SubprocessFailure(t *testing.T) {
	p := &provider.CLISubprocessProvider{
		Kind: provider.CLIKindCodex,
		Run: func(_ context.Context, name string, args []string, dir string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	}

	_, err := drainStreamTurn(t, p)
	if err == nil {
		t.Fatal("expected an error when the subprocess itself fails to run")
	}
}

func TestDetectCLI(t *testing.T) {
	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "claude")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}

	t.Setenv("PATH", tempDir)
	if !provider.DetectCLI(provider.CLIKindClaude) {
		t.Fatal("expected DetectCLI to find the fake claude binary on PATH")
	}
	if provider.DetectCLI(provider.CLIKindCodex) {
		t.Fatal("expected DetectCLI to report codex as unavailable when it isn't on PATH")
	}
}
