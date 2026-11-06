package provider

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/agent"
)

// ACP-1: an agent that answers nothing at all — opencode's silent retry loop
// once its plan's usage limit is reached — must not leave the turn waiting
// forever. The turn ends with a message the server reads as a usage limit.
func TestACP_SilentAgentEndsTheTurn(t *testing.T) {
	old := acpSilenceTimeout
	acpSilenceTimeout = 150 * time.Millisecond
	t.Cleanup(func() { acpSilenceTimeout = old })

	var args []string
	rec := &turnRecorder{}
	p := &CLISubprocessProvider{Kind: CLIKindOpenCode, Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{"protocolVersion": 1}
			case "session/new":
				return map[string]any{"sessionId": "s1"}
			case "session/prompt":
				return nil // and then nothing, ever: no update, no response
			}
			return nil
		})
	})
	start := time.Now()
	_, err := runNative(t, p, rec.turn(agent.ModeReadWrite))
	wait()
	if err == nil {
		t.Fatal("a silent agent ended the turn with no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the turn waited %s for an agent that said nothing", time.Since(start))
	}
	// The wording has to reach the server's limit classifier as a usage limit
	// (cmd/yoakeland limit.go), or the user is left with a bare error.
	var silent *acpSilentError
	if !errors.As(err, &silent) || !silent.UsageLimit() {
		t.Fatalf("error %v isn't a usage-limit silence", err)
	}
}

// A tool call the agent opened and hasn't finished is a reason to be quiet: a
// long build must not be cut off as if the agent had hung.
func TestACP_OpenToolCallIsNotSilence(t *testing.T) {
	old := acpSilenceTimeout
	acpSilenceTimeout = 200 * time.Millisecond
	t.Cleanup(func() { acpSilenceTimeout = old })

	var args []string
	rec := &turnRecorder{}
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		update := func(u map[string]any) {
			line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
		}
		rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{"protocolVersion": 1}
			case "session/new":
				return map[string]any{"sessionId": "s1"}
			case "session/prompt":
				go func(id any) {
					update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "Build", "kind": "execute", "status": "in_progress"})
					time.Sleep(3 * acpSilenceTimeout) // nothing at all while it runs
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed",
						"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "built"}}}})
					line(out, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
				}(msg["id"])
				return nil
			}
			return nil
		})
	})
	chunks, err := runNative(t, p, rec.turn(agent.ModeReadWrite))
	wait()
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if k := kinds(chunks); !strings.Contains(k, "tool_call") || !strings.Contains(k, "tool_result") {
		t.Fatalf("chunks = %s", k)
	}
}

// A turn that says nothing for a while but is working is not cut off: any
// update restarts the clock.
func TestACP_ActivityKeepsTheTurnAlive(t *testing.T) {
	old := acpSilenceTimeout
	acpSilenceTimeout = 200 * time.Millisecond
	t.Cleanup(func() { acpSilenceTimeout = old })

	var args []string
	rec := &turnRecorder{}
	p := &CLISubprocessProvider{Kind: CLIKindKimi, Dir: t.TempDir()}
	var wait func()
	p.Start, wait = fakeCLI(t, &args, func(in *bufio.Scanner, out io.Writer) {
		update := func(u map[string]any) {
			line(out, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
		}
		rpcServe(in, out, func(msg map[string]any) any {
			switch msg["method"] {
			case "initialize":
				return map[string]any{"protocolVersion": 1}
			case "session/new":
				return map[string]any{"sessionId": "s1"}
			case "session/prompt":
				go func(id any) {
					// Quiet for longer than the timeout, in three pieces, each
					// shorter than it: a working agent, not a silent one.
					for _, text := range []string{"still ", "work", "ing"} {
						time.Sleep(3 * acpSilenceTimeout / 4)
						update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
					}
					line(out, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
				}(msg["id"])
				return nil
			}
			return nil
		})
	})
	chunks, err := runNative(t, p, rec.turn(agent.ModeReadWrite))
	wait()
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.Delta)
	}
	if text.String() != "still working" {
		t.Fatalf("streamed %q", text.String())
	}
}

// An open tool call buys time, but not forever: one that never finishes can't
// hold the turn past acpBusySilenceTimeout.
func TestSilenceWatch_BusyIsBounded(t *testing.T) {
	oldQuiet, oldBusy := acpSilenceTimeout, acpBusySilenceTimeout
	acpSilenceTimeout, acpBusySilenceTimeout = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { acpSilenceTimeout, acpBusySilenceTimeout = oldQuiet, oldBusy })

	w := newSilenceWatch()
	defer w.Stop()
	if !w.expired(false) {
		t.Fatal("quiet and not busy should give up")
	}
	if w.expired(true) {
		t.Fatal("quiet but busy should wait")
	}
	time.Sleep(60 * time.Millisecond)
	if !w.expired(true) {
		t.Fatal("busy for longer than acpBusySilenceTimeout should give up")
	}
	w.hear()
	if w.expired(true) {
		t.Fatal("hearing from the agent should restart the busy allowance")
	}
}
