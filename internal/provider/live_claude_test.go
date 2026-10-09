package provider

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// Live: drives the real claude binary through the provider (skipped unless YK_LIVE=1).
func TestLiveClaudeSession(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir()}
	ask := func(mode agent.Mode, resume, prompt string) (string, *agent.NativeTurn) {
		rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
		nt := rec.turn(mode)
		nt.SessionKey, nt.ResumeID, nt.Prompt = key, resume, prompt
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
		var sb strings.Builder
		for c := range chunks {
			if c.Kind == agent.StreamChunkDelta {
				sb.WriteString(c.Delta)
			}
			if c.Kind == agent.StreamChunkUsage {
				t.Logf("usage: %+v", c.Usage)
			}
		}
		if err := <-errs; err != nil {
			t.Fatalf("turn: %v", err)
		}
		return sb.String(), nt
	}
	pids := func() string {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
		return strings.TrimSpace(string(out))
	}
	a1, nt1 := ask(agent.ModeReadWrite, "", "Remember the word ZEBRA. Reply OK.")
	pid1 := pids()
	a2, nt2 := ask(agent.ModeReadWrite, nt1.SessionID, "What word did I ask you to remember? One word.")
	pid2 := pids()
	t.Logf("turn1=%q session=%s pid=%s | turn2=%q session=%s pid=%s", a1, nt1.SessionID, pid1, a2, nt2.SessionID, pid2)
	if pid1 == "" || pid1 != pid2 {
		t.Fatalf("process not reused: %q vs %q", pid1, pid2)
	}
	if !strings.Contains(strings.ToUpper(a2), "ZEBRA") {
		t.Fatalf("second turn forgot: %q", a2)
	}
	a3, _ := ask(agent.ModeAcceptEdits, nt1.SessionID, "Say hi in one word.")
	t.Logf("after in-session mode switch: %q pid=%s", a3, pids())
	if pids() != pid1 {
		t.Fatalf("mode switch restarted the process")
	}
}

// Live: a message steered into a running turn is folded into it (one turn, one result).
func TestLiveClaudeSteer(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir()}
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
	nt := rec.turn(agent.ModeReadWrite)
	nt.SessionKey, nt.Prompt = key, "Run this bash command: sleep 8 && echo finished. Then reply with just the word DONE."
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
	var text strings.Builder
	steered := false
	for c := range chunks {
		switch c.Kind {
		case agent.StreamChunkToolCall:
			if !steered {
				steered = true
				time.Sleep(1500 * time.Millisecond)
				if err := nt.Steer("Also, when you reply, append the word PINEAPPLE."); err != nil {
					t.Errorf("steer: %v", err)
				}
			}
		case agent.StreamChunkDelta:
			text.WriteString(c.Delta)
		}
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	t.Logf("reply = %q", text.String())
	if !steered || !strings.Contains(strings.ToUpper(text.String()), "PINEAPPLE") {
		t.Fatalf("the steer didn't reach the running turn: steered=%v reply=%q", steered, text.String())
	}
}

// Live: Auto runs a harmless command without asking on a model that supports it,
// and a model that doesn't support it refuses the mode instead of running unreviewed.
func TestLiveClaudeAuto(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	run := func(model string, mode agent.Mode) (string, int, error) {
		key := t.Name() + model
		defer CloseNativeSessions(key)
		p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: model, Dir: t.TempDir()}
		asks := 0
		rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
		nt := rec.turn(mode)
		nt.SessionKey, nt.Prompt = key, "Run the bash command `echo hello-auto` and tell me what it printed."
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
		var text strings.Builder
		for c := range chunks {
			if c.Kind == agent.StreamChunkDelta {
				text.WriteString(c.Delta)
			}
		}
		err := <-errs
		rec.mu.Lock()
		asks = len(rec.asks)
		rec.mu.Unlock()
		return text.String(), asks, err
	}
	text, asks, err := run("sonnet", agent.ModeAuto)
	t.Logf("sonnet/auto: %q asks=%d err=%v", text, asks, err)
	if err != nil || asks != 0 || !strings.Contains(text, "hello-auto") {
		t.Fatalf("auto on sonnet: %q asks=%d err=%v", text, asks, err)
	}
	_, _, err = run("haiku", agent.ModeAuto)
	t.Logf("haiku/auto: err=%v", err)
	if err == nil {
		t.Fatal("haiku accepted auto mode")
	}
}

// Live: a turn forked at an earlier turn's checkpoint remembers only what came before it.
func TestLiveClaudeForkAtCheckpoint(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir()}
	ask := func(resume, forkAt, prompt string) (string, *agent.NativeTurn) {
		rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
		nt := rec.turn(agent.ModeReadWrite)
		nt.SessionKey, nt.ResumeID, nt.ForkAt, nt.Prompt, nt.FreshPrompt = key, resume, forkAt, prompt, prompt
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
		var sb strings.Builder
		for c := range chunks {
			if c.Kind == agent.StreamChunkDelta {
				sb.WriteString(c.Delta)
			}
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		return sb.String(), nt
	}
	_, nt1 := ask("", "", "Remember the code word ZEBRA. Reply OK.")
	_, nt2 := ask(nt1.SessionID, "", "Remember a second code word: LLAMA. Reply OK.")
	t.Logf("turn 1: session %s checkpoint %s | turn 2: session %s checkpoint %s", nt1.SessionID, nt1.Checkpoint, nt2.SessionID, nt2.Checkpoint)
	if nt1.Checkpoint == "" || nt2.Checkpoint == "" || nt1.Checkpoint == nt2.Checkpoint {
		t.Fatalf("checkpoints: %q %q", nt1.Checkpoint, nt2.Checkpoint)
	}
	got, nt3 := ask(nt1.SessionID, nt1.Checkpoint, "What code words do you remember? One line.")
	t.Logf("forked at turn 1: %q (session %s)", got, nt3.SessionID)
	if nt3.SessionID == "" || nt3.SessionID == nt1.SessionID {
		t.Fatalf("the fork kept the old session id: %q", nt3.SessionID)
	}
	if !strings.Contains(strings.ToUpper(got), "ZEBRA") || strings.Contains(strings.ToUpper(got), "LLAMA") {
		t.Fatalf("the fork remembered the wrong things: %q", got)
	}
}

// Live: the real claude binary's get_context_usage, after a real turn (skipped unless YK_LIVE=1).
// Checks what the parser assumes of the answer: the categories add up to the total, the window is
// known, and once the API has answered the total is the provider's own count.
func TestLiveClaudeContextBreakdown(t *testing.T) {
	if os.Getenv("YK_LIVE") == "" {
		t.Skip("YK_LIVE not set")
	}
	key := t.Name()
	t.Cleanup(func() { CloseNativeSessions(key) })
	p := &CLISubprocessProvider{Kind: CLIKindClaude, Model: "haiku", Dir: t.TempDir()}
	rec := &turnRecorder{answer: agent.NativeAnswer{Allowed: true}}
	nt := rec.turn(agent.ModeReadWrite)
	nt.SessionKey, nt.Prompt = key, "Reply with the single word OK."
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	chunks, errs := p.StreamTurn(agent.WithNativeTurn(ctx, nt), nil, nil, "")
	var usage *protocol.UsageData
	for c := range chunks {
		if c.Kind == agent.StreamChunkUsage {
			usage = c.Usage
		}
	}
	// An expired login fails the turn but leaves the process able to count its context, which is
	// still worth checking; only what needs a real reply is skipped then.
	turnErr := <-errs
	if turnErr != nil {
		t.Logf("the claude turn failed (is the login current?): %v", turnErr)
	}
	t.Logf("turn usage: %+v", usage)

	got, err := p.ContextBreakdown(ctx, key)
	if err != nil || got == nil {
		t.Fatalf("breakdown = %+v, %v", got, err)
	}
	used, spare := sumTokens(got.Categories)
	t.Logf("total %d window %d compact-at %d source %s cache %+v; used %d spare %d", got.TotalTokens, got.WindowTokens, got.AutoCompactAt, got.Source, got.Cache, used, spare)
	for _, c := range got.Categories {
		t.Logf("  %-14s %8d exact=%v", c.Key, c.Tokens, c.Exact)
	}
	if got.TotalTokens == 0 || got.WindowTokens == 0 {
		t.Fatalf("no total or window: %+v", got)
	}
	if d := used - got.TotalTokens; d < -got.TotalTokens/100 || d > got.TotalTokens/100 {
		t.Fatalf("categories add up to %d, the total is %d", used, got.TotalTokens)
	}
	if turnErr != nil {
		return
	}
	if got.Source != "provider" || got.Cache == nil {
		t.Fatalf("after a real reply the count is the provider's, with its cache use: %+v", got)
	}
	if usage != nil && usage.ContextTokens > 0 {
		// Both are the last request's size; the CLI's count may include a few tokens the API didn't bill.
		if d := usage.ContextTokens - got.TotalTokens; d < -got.TotalTokens/5 || d > got.TotalTokens/5 {
			t.Errorf("turn snapshot says %d in context, get_context_usage says %d", usage.ContextTokens, got.TotalTokens)
		}
	}
}
