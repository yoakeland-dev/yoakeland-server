package sandbox

import (
	"sync/atomic"
	"time"
)

// Limits are the sandbox's runtime-tunable caps. The daemon pushes them from
// its runtime settings; a zero field keeps its default.
type Limits struct {
	ExecTimeout          time.Duration // one-shot bash command wall clock
	ExecOutputMaxBytes   int           // kept per stream of a one-shot command
	TerminalMemoryBytes  int64         // cgroup / job-object cap per terminal
	TerminalPids         int           // process-count cap per terminal
	TerminalReadMaxChars int           // tail of a terminal read the model sees
	TerminalHistoryBytes int           // scrollback replayed on attach
}

var limits atomic.Pointer[Limits]

// SetLimits replaces the caps. Commands and terminals already running keep
// the ones they started with.
func SetLimits(l Limits) { limits.Store(&l) }

// CurrentLimits is the caps in force, defaults filled in.
func CurrentLimits() Limits {
	var l Limits
	if p := limits.Load(); p != nil {
		l = *p
	}
	if l.ExecTimeout <= 0 {
		l.ExecTimeout = 60 * time.Second
	}
	if l.ExecOutputMaxBytes <= 0 {
		l.ExecOutputMaxBytes = maxCapturedOutput
	}
	if l.TerminalMemoryBytes <= 0 {
		l.TerminalMemoryBytes = 2 << 30
	}
	if l.TerminalPids <= 0 {
		l.TerminalPids = 512
	}
	if l.TerminalReadMaxChars <= 0 {
		l.TerminalReadMaxChars = 12000
	}
	if l.TerminalHistoryBytes <= 0 {
		l.TerminalHistoryBytes = 256 * 1024
	}
	return l
}

// execTimeout is the one-shot wall clock: the runtime limit once one has been
// set, else the timeout the sandbox was built with.
func execTimeout(def time.Duration) time.Duration {
	if p := limits.Load(); p != nil && p.ExecTimeout > 0 {
		return p.ExecTimeout
	}
	return def
}
