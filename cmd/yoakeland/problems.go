package main

import (
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

const (
	slowToolThreshold = audit.SlowTool
	problemTextMax    = 400 // runes kept of any cause or message
	warningRingSize   = 200
	problemWarnMax    = 30
	problemFailMax    = 20
	problemSlowMax    = 10
)

// loggedWarning is one WARN or ERROR line kept for list_problems.
type loggedWarning struct {
	at      time.Time
	level   string
	message string
}

// warnings is the ring of recent WARN/ERROR lines, fed by logLine.
var warnings struct {
	sync.Mutex
	items []loggedWarning
}

// recordWarning keeps a WARN or ERROR line for list_problems. The text is
// redacted here, so the raw secret never sits in memory.
func recordWarning(level, msg string) {
	msg = clipText(audit.Redact(msg))
	warnings.Lock()
	defer warnings.Unlock()
	warnings.items = append(warnings.items, loggedWarning{at: time.Now(), level: level, message: msg})
	if n := len(warnings.items); n > warningRingSize {
		// A copy, so the old backing array (and the entries it still holds) can go.
		warnings.items = append([]loggedWarning(nil), warnings.items[n-warningRingSize:]...)
	}
}

// recentWarnings returns the buffered warnings, newest first.
func recentWarnings() []loggedWarning {
	warnings.Lock()
	defer warnings.Unlock()
	out := make([]loggedWarning, len(warnings.items))
	for i, w := range warnings.items {
		out[len(out)-1-i] = w
	}
	return out
}

// handleListProblems answers list_problems. Local connections only: causes can
// hold secrets. The audit read is capped at audit.MaxLimit tool runs (newest).
func handleListProblems(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) {
	events, _, err := deps.audit.Read(audit.Query{Kinds: []string{audit.ToolExec}, Limit: audit.MaxLimit})
	if err != nil {
		logWarn("list_problems: reading the audit log: %v", err)
	}
	_ = ipc.SendEvent(protocol.EventProblemList, "", "", "", buildProblems(time.Now(), events, recentWarnings()))
}

// buildProblems turns tool-run audit events and warnings, both newest first,
// into a problem_list. A tool run counts as failed when its outcome is "error",
// with its cause in detail.error and its duration in detail.durationMs; agent.auditResult
// writes those lines only for failed or slow runs.
func buildProblems(now time.Time, events []audit.Event, warns []loggedWarning) protocol.ProblemListData {
	threshold := slowToolThreshold.Milliseconds()
	out := protocol.ProblemListData{
		ReadAtMs:        now.UnixMilli(),
		SlowThresholdMs: threshold,
		LatestFailures:  []protocol.ProblemFailure{},
		Slowest:         []protocol.ProblemSlow{},
		Warnings:        []protocol.ProblemWarning{},
	}
	var timed []protocol.ProblemSlow
	for _, e := range events {
		if e.Outcome == "run" { // the "error" and "slow" lines follow a run already counted
			out.ToolCalls++
		}
		dur := detailNumber(e.Detail, "durationMs")
		atMs := e.At.UnixMilli()
		if e.Outcome == "error" {
			out.FailedCount++
			if len(out.LatestFailures) < problemFailMax {
				out.LatestFailures = append(out.LatestFailures, protocol.ProblemFailure{
					Tool: e.Tool, Cause: clipText(audit.Redact(detailString(e.Detail, "error"))), DurationMs: dur, AtMs: atMs,
				})
			}
		}
		if dur > 0 {
			timed = append(timed, protocol.ProblemSlow{Tool: e.Tool, DurationMs: dur, AtMs: atMs})
			if dur >= threshold {
				out.SlowCount++
			}
		}
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].DurationMs > timed[j].DurationMs })
	out.Slowest = append(out.Slowest, timed[:min(len(timed), problemSlowMax)]...)

	out.WarnCount = len(warns)
	for _, w := range warns {
		if len(out.Warnings) == problemWarnMax {
			break
		}
		out.Warnings = append(out.Warnings, protocol.ProblemWarning{AtMs: w.at.UnixMilli(), Level: w.level, Message: clipText(w.message)})
	}
	return out
}

func detailNumber(d map[string]any, key string) int64 {
	switch v := d[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func detailString(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

// clipText keeps the first problemTextMax runes of s.
func clipText(s string) string {
	if utf8.RuneCountInString(s) <= problemTextMax {
		return s
	}
	return string([]rune(s)[:problemTextMax]) + "…"
}
