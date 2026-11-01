package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

func requireProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
}

func TestSignalProcess_RefusesSelfAndNonDescendants(t *testing.T) {
	requireProc(t)
	self := os.Getpid()
	for _, pid := range []int{self, 1, 2147483646} {
		for _, sig := range []string{"SIGINT", "SIGKILL"} {
			out := signalProcess(self, pid, sig)
			if out.Signaled {
				t.Fatalf("pid %d %s: signaled, want refused", pid, sig)
			}
			if !strings.Contains(out.Message, "not a live descendant") {
				t.Fatalf("pid %d %s: message %q lacks \"not a live descendant\"", pid, sig, out.Message)
			}
		}
	}
}

func TestSignalProcess_RejectsOtherSignals(t *testing.T) {
	requireProc(t)
	// A real child is used, so the only reason to refuse is the signal name.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	out := signalProcess(os.Getpid(), cmd.Process.Pid, "SIGTERM")
	if out.Signaled || !strings.Contains(out.Message, "SIGINT") {
		t.Fatalf("SIGTERM: got %+v, want refused with a message naming the allowed signals", out)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)); err != nil {
		t.Fatal("refused signal, but the child is gone")
	}
}

func TestSignalProcess_SignalsLiveDescendant(t *testing.T) {
	requireProc(t)
	if runtime.GOOS == "windows" {
		t.Skip("uses unix signals")
	}
	for _, sig := range []string{"SIGINT", "SIGKILL"} {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Skip("cannot start sleep:", err)
		}
		out := signalProcess(os.Getpid(), cmd.Process.Pid, sig)
		if !out.Signaled {
			_ = cmd.Process.Kill()
			t.Fatalf("%s: not signaled: %+v", sig, out)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: child exited cleanly, want a signal exit", sig)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s: child still running after signal", sig)
		}
	}
}

func TestSignalProcess_LocalOnly(t *testing.T) {
	line := envelopeJSON(t, protocol.EventSignalProcess, protocol.SignalProcessData{PID: os.Getpid(), Signal: "SIGKILL"})
	envs := runDispatchNetwork(t, newTestDeps(t), "tcp", line)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("tcp signal_process: want one error, got %+v", envs)
	}
	var e protocol.ErrorData
	_ = json.Unmarshal(envs[0].Data, &e)
	if !strings.Contains(e.Message, "signal_process") {
		t.Fatalf("error message %q", e.Message)
	}
}

func TestListProblems_LocalOnly(t *testing.T) {
	line := envelopeJSON(t, protocol.EventListProblems, struct{}{})
	envs := runDispatchNetwork(t, newTestDeps(t), "tcp", line)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("tcp list_problems: want one error, got %+v", envs)
	}
}

func TestListProblems_AnswersOverTheLocalSocket(t *testing.T) {
	deps, log, _ := auditedDeps(t)
	log.Append(audit.Event{Kind: audit.ToolExec, Tool: "bash", Outcome: "run"})
	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListProblems, struct{}{}))
	if len(envs) != 1 || envs[0].Type != protocol.EventProblemList {
		t.Fatalf("want one problem_list, got %+v", envs)
	}
	var out protocol.ProblemListData
	if err := json.Unmarshal(envs[0].Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolCalls != 1 || out.SlowThresholdMs != 10000 || out.LatestFailures == nil || out.Warnings == nil {
		t.Fatalf("unexpected problem_list: %+v", out)
	}
}

func TestBuildProblems_FromFixture(t *testing.T) {
	now := time.UnixMilli(1790000000000)
	at := func(s int) time.Time { return now.Add(-time.Duration(s) * time.Second) }
	events := []audit.Event{ // newest first, as audit.Read returns them
		{Kind: audit.ToolExec, Tool: "bash", Outcome: "error", At: at(1),
			Detail: map[string]any{"error": "exit 1 password=hunter2222", "durationMs": float64(1500)}},
		{Kind: audit.ToolExec, Tool: "write", Outcome: "slow", At: at(2),
			Detail: map[string]any{"durationMs": float64(12000)}},
		{Kind: audit.ToolExec, Tool: "bash", Outcome: "error", At: at(3),
			Detail: map[string]any{"error": strings.Repeat("x", 500), "durationMs": float64(20000)}},
		{Kind: audit.ToolExec, Tool: "edit", Outcome: "run", At: at(4)},
		{Kind: audit.ToolExec, Tool: "write", Outcome: "run", At: at(5)},
	}
	warns := make([]loggedWarning, 35)
	for i := range warns {
		warns[i] = loggedWarning{at: at(i), level: "WARN", message: "w"}
	}

	out := buildProblems(now, events, warns)

	if out.ReadAtMs != now.UnixMilli() || out.ToolCalls != 2 || out.FailedCount != 2 {
		t.Fatalf("counts: %+v", out)
	}
	if len(out.LatestFailures) != 2 || out.LatestFailures[0].Tool != "bash" || out.LatestFailures[0].AtMs != at(1).UnixMilli() {
		t.Fatalf("latestFailures not newest first: %+v", out.LatestFailures)
	}
	if strings.Contains(out.LatestFailures[0].Cause, "hunter2222") {
		t.Fatalf("cause not redacted: %q", out.LatestFailures[0].Cause)
	}
	if n := len([]rune(out.LatestFailures[1].Cause)); n != problemTextMax+1 {
		t.Fatalf("cause clipped to %d runes, want %d", n, problemTextMax+1)
	}
	if out.SlowCount != 2 {
		t.Fatalf("slowCount = %d, want 2", out.SlowCount)
	}
	if len(out.Slowest) != 3 || out.Slowest[0].DurationMs != 20000 || out.Slowest[1].DurationMs != 12000 || out.Slowest[2].DurationMs != 1500 {
		t.Fatalf("slowest not longest first: %+v", out.Slowest)
	}
	if out.WarnCount != 35 || len(out.Warnings) != problemWarnMax {
		t.Fatalf("warnCount=%d warnings=%d, want 35 and %d", out.WarnCount, len(out.Warnings), problemWarnMax)
	}
	b, _ := json.Marshal(buildProblems(now, nil, nil))
	if !bytes.Contains(b, []byte(`"latestFailures":[]`)) || !bytes.Contains(b, []byte(`"warnings":[]`)) {
		t.Fatalf("empty lists must marshal as [], got %s", b)
	}
}

func TestRecordWarning_RedactsAndBoundsTheRing(t *testing.T) {
	recordWarning("WARN", "probe-line-7731 token=abc123secret")
	for _, w := range recentWarnings() {
		if strings.Contains(w.message, "abc123secret") {
			t.Fatalf("secret kept in warning ring: %q", w.message)
		}
	}
	for i := 0; i < warningRingSize+50; i++ {
		recordWarning("ERROR", "flood")
	}
	if n := len(recentWarnings()); n > warningRingSize {
		t.Fatalf("ring holds %d, want at most %d", n, warningRingSize)
	}
}
