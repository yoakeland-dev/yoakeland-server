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
