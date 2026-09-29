package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// Amp, driven through its streaming-JSON session (SPEC.md §6b):
//
//	amp [threads continue <id>] --execute --stream-json-thinking --stream-json-input
//	    --dangerously-allow-all [--mode m] [--effort e] [--fast]
//
// It reads newline-delimited user messages from stdin and stays alive until the
// assistant is done and stdin closes, so one process serves the chat. The stream
// is Claude Code's format, with three differences that matter, all from
// probing the real CLI (see Waku's docs/providers.md): there is no `result`, a
// turn is over when an assistant message stops with `end_turn` (a `tool_use`
// stop is mid-turn); text arrives as whole blocks, not deltas; and there is no
// permission request and no interrupt. A message written mid-turn is held until
// the turn ends unless it carries `"steer": true`, which has the running turn
// absorb it.
//
// Amp is untested against the real CLI here (none installed): this follows the
// behaviour Waku observed.

// ampModels are Amp's agent modes, which stand in for models.
func ampModels() []ModelInfo {
	var out []ModelInfo
	for _, m := range []struct{ id, name string }{{"low", "Low"}, {"medium", "Medium"}, {"high", "High"}, {"ultra", "Ultra"}} {
		out = append(out, ModelInfo{ID: m.id, DisplayName: m.name, Capabilities: map[string]any{"fastMode": true}})
	}
	return out
}

// ampModes: Amp is launched with --dangerously-allow-all and shows no permission
// request, so full access is the only mode it can honestly offer.
func ampModes() []ModeSpec {
	return []ModeSpec{{Mode: string(agent.ModeAutonomous), Policy: polAutonomous, EnforcedBy: "agent", Native: "--dangerously-allow-all"}}
}

type ampSession struct {
	proc   *nativeProc
	wmu    sync.Mutex
	exited chan struct{}

	mu     sync.Mutex
	turn   *ampTurn
	thread string // Amp's thread id
}

func (s *ampSession) write(v any) error { return writeJSONLine(s.proc.stdin, &s.wmu, v) }

func (s *ampSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

func (s *ampSession) close() {
	_ = s.proc.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		if s.proc.kill != nil {
			s.proc.kill()
		}
	}
}

func (s *ampSession) setTurn(t *ampTurn) {
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
}

func (s *ampSession) read() {
	for s.proc.lines.Scan() {
		var ln claudeLine
		if json.Unmarshal(s.proc.lines.Bytes(), &ln) != nil {
			continue
		}
		s.mu.Lock()
		if ln.Type == "system" && ln.Subtype == "init" && ln.SessionID != "" {
			s.thread = ln.SessionID
		}
		t := s.turn
		s.mu.Unlock()
		if t != nil {
			t.handle(ln)
		}
	}
	close(s.exited)
	_ = s.proc.wait()
}

// ampTurn is what one turn has seen of the stream.
type ampTurn struct {
	nt   *agent.NativeTurn
	emit func(agent.ModelStreamChunk)
	done chan struct{} // the turn ended
	over atomic.Bool
	seen atomic.Bool

	mu    sync.Mutex
	names map[string]string
	usage protocol.UsageData
	err   string
}

func (t *ampTurn) finish() {
	if t.over.Swap(true) {
		return
	}
	close(t.done)
}

func (t *ampTurn) handle(ln claudeLine) {
	t.seen.Store(true)
	topLevel := ln.ParentToolUseID == nil || *ln.ParentToolUseID == ""
	switch ln.Type {
	case "system":
		if ln.Subtype == "init" && ln.SessionID != "" && t.nt.SessionID == "" {
			t.nt.SessionID = ln.SessionID
		}
		if ln.Error != "" {
			t.mu.Lock()
			t.err = ln.Error
			t.mu.Unlock()
		}
	case "assistant":
		for _, b := range ln.Message.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: b.Text})
				}
			case "thinking":
				if b.Thinking != "" {
					t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: b.Thinking})
				}
			case "tool_use":
				t.mu.Lock()
				t.names[b.ID] = b.Name
				t.mu.Unlock()
				tc := claudeToolCall(b.ID, b.Name, b.Input)
				if !topLevel {
					tc.ParentToolCallID = *ln.ParentToolUseID
				}
				t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &tc})
			}
		}
		if topLevel {
			u := ln.Message.Usage
			in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
			t.mu.Lock()
			t.usage.InputTokens += in
			t.usage.OutputTokens += u.OutputTokens
			t.usage.CachedInputTokens += u.CacheReadInputTokens
			t.mu.Unlock()
			// The assistant stopping for its own reasons, not to call a tool, ends the turn.
			if ln.Message.StopReason == "end_turn" {
				t.finish()
			}
		}
	case "user":
		for _, b := range ln.Message.Content {
			if b.Type != "tool_result" {
				continue
			}
			t.mu.Lock()
			name := t.names[b.ToolUseID]
			t.mu.Unlock()
			r := &agent.NativeToolResult{ToolCallID: b.ToolUseID, Name: name}
			if !topLevel {
				r.ParentToolCallID = *ln.ParentToolUseID
			}
			if text := claudeResultText(b.Content); b.IsError {
				r.Error = text
			} else {
				r.Output = text
			}
			t.emit(agent.ModelStreamChunk{Kind: agent.StreamChunkToolResult, ToolResult: r})
		}
	case "result":
		// Amp isn't seen to send one, but if it reports a failure there is no end_turn to wait for.
		if ln.IsError {
			t.mu.Lock()
			t.err = strings.TrimSpace(ln.Result)
			if t.err == "" {
				t.err = ln.Error
			}
			t.mu.Unlock()
			t.finish()
		}
	}
}

func (p *CLISubprocessProvider) ampArgs(threadID string) []string {
	var args []string
	if threadID != "" {
		args = append(args, "threads", "continue", threadID)
	}
	// --stream-json-thinking implies --stream-json, which --stream-json-input needs.
	args = append(args, "--execute", "--stream-json-thinking", "--stream-json-input", "--dangerously-allow-all")
	if p.Model != "" {
		args = append(args, "--mode", p.Model)
	}
	if p.effort != "" {
		args = append(args, "--effort", p.effort)
	}
	if p.fast {
		args = append(args, "--fast")
	}
	return args
}

func (p *CLISubprocessProvider) runAmp(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	if nt.Mode != agent.ModeAutonomous {
		return errors.New("amp runs with full access only: switch this chat to Run")
	}
	err := p.runAmpOnce(ctx, nt, emit)
	if errors.Is(err, errStaleSession) {
		err = p.runAmpOnce(ctx, nt, emit)
	}
	return err
}

func (p *CLISubprocessProvider) runAmpOnce(ctx context.Context, nt *agent.NativeTurn, emit func(agent.ModelStreamChunk)) error {
	// Mode, effort and fast are launch arguments, so a change to any restarts (resuming the thread).
	sig := strings.Join([]string{p.Dir, p.Model, p.effort, fmt.Sprint(p.fast)}, "\x00")
	usable := func(ns nativeSession) bool {
		s := ns.(*ampSession)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.thread == nt.ResumeID
	}
	ns, reused, release, err := acquireSession(nt, CLIKindAmp, sig, usable, func() (nativeSession, error) {
		proc, err := p.start()(context.Background(), "amp", p.ampArgs(nt.ResumeID), p.Dir)
		if err != nil {
			return nil, err
		}
		s := &ampSession{proc: proc, exited: make(chan struct{}), thread: nt.ResumeID}
		go s.read()
		return s, nil
	})
	if err != nil {
		return err
	}
	defer release()
	s := ns.(*ampSession)

	t := &ampTurn{nt: nt, emit: emit, done: make(chan struct{}), names: map[string]string{}}
	s.setTurn(t)
	defer s.setTurn(nil)

	userLine := func(text string, steer bool) map[string]any {
		m := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}}
		if steer {
			m["steer"] = true
		}
		return m
	}
	prompt := nt.Prompt
	if nt.ResumeID == "" {
		prompt = nt.FreshPrompt
	}
	if err := s.write(userLine(prompt, false)); err != nil {
		if reused && s.dead() {
			return errStaleSession
		}
		return fmt.Errorf("amp: %w", err)
	}
	// Without the marker Amp would hold the message until end_turn and run it as a turn of its own.
	defer t.over.Store(true)
	nt.SetSteer(func(text string) error {
		if t.over.Load() || s.dead() {
			return errors.New("the turn has ended")
		}
		return s.write(userLine(text, true))
	})

	select {
	case <-t.done:
	case <-s.exited:
		select {
		case <-t.done:
		default:
			if reused && !t.seen.Load() {
				return errStaleSession
			}
			msg := "amp exited before the turn ended"
			if tail := s.proc.stderr(); tail != "" {
				msg += ": " + lastLines(tail, 5)
			}
			return errors.New(msg)
		}
	case <-ctx.Done():
		// Amp has no interrupt on the stream: stopping means ending the process. The thread
		// survives on Amp's side and the next turn continues it.
		if s.proc.kill != nil {
			s.proc.kill()
		} else {
			s.close()
		}
		return ctx.Err()
	}

	if s.thread != "" {
		s.mu.Lock()
		nt.SessionID = s.thread
		s.mu.Unlock()
	}
	t.mu.Lock()
	u, msg := t.usage, t.err
	t.mu.Unlock()
	u.TotalTokens = u.InputTokens + u.OutputTokens
	emit(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &u}) // no cost reported
	if msg != "" {
		return fmt.Errorf("amp: %s", msg)
	}
	return nil
}
