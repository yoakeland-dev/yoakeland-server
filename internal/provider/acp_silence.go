package provider

import (
	"fmt"
	"time"
)

// acpSilenceTimeout is how long an ACP turn waits for any sign of life before
// it stops waiting and ends the turn. Generous — an agent may think or run a
// tool quietly for minutes — but finite: an agent that hits its plan's limit
// and starts retrying in the background answers nothing and sends nothing, so
// without this the turn would simply never end. A var so tests can shrink it.
var acpSilenceTimeout = 5 * time.Minute

// acpBusySilenceTimeout is the same wait while the agent has a tool call open,
// which explains a quiet stretch (a long build) but not an endless one.
var acpBusySilenceTimeout = 30 * time.Minute

// silenceWatch tells an agent that is working quietly from one that has
// stopped answering: hear() on every sign of life, and when C fires, expired()
// says whether to give up.
type silenceWatch struct {
	timer *time.Timer
	heard time.Time
}

func newSilenceWatch() *silenceWatch {
	return &silenceWatch{timer: time.NewTimer(acpSilenceTimeout), heard: time.Now()}
}

// C fires once the agent has been quiet for acpSilenceTimeout.
func (w *silenceWatch) C() <-chan time.Time { return w.timer.C }

func (w *silenceWatch) Stop() { w.timer.Stop() }

// hear restarts the clock: the agent said something.
func (w *silenceWatch) hear() {
	if !w.timer.Stop() {
		select {
		case <-w.timer.C:
		default:
		}
	}
	w.heard = time.Now()
	w.timer.Reset(acpSilenceTimeout)
}

// expired is called when C has fired. An open tool call buys more time, up to
// acpBusySilenceTimeout since the agent last spoke; otherwise the wait is over.
func (w *silenceWatch) expired(busy bool) bool {
	if busy && time.Since(w.heard) < acpBusySilenceTimeout {
		w.timer.Reset(acpSilenceTimeout)
		return false
	}
	return true
}

// acpSilentError ends a turn whose agent went quiet. For opencode the silence is
// its retry loop on a spent free plan, so the server reads it as a usage limit
// (UsageLimit, see cmd/yoakeland limit.go) rather than as a mystery error.
type acpSilentError struct {
	binary string
	quiet  time.Duration
	limit  bool
}

func (e *acpSilentError) Error() string {
	if e.limit {
		return fmt.Sprintf("%s stopped answering (%s with nothing sent back); it does this when the plan's usage limit is reached, retrying the rejected request on its own schedule", e.binary, e.quiet)
	}
	return fmt.Sprintf("%s sent nothing for %s; the turn was stopped", e.binary, e.quiet)
}

func (e *acpSilentError) UsageLimit() bool { return e.limit }
