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
