package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yoakeland/internal/protocol"
)

// What the server does when a turn stops because a provider's usage limit ran out (SPEC.md §3.1
// limit_continue). The failed turn is detected from its error, the chat gets a LimitHit that every
// client shows as a card, and, if the person asked for it, a scheduled turn continues the work once
// the limit resets. The hit and the scheduled turn are stored with the schedule, so a restart keeps
// both.
const (
	limitMaxWait      = 8 * 24 * time.Hour // a week and a day; past this the server doesn't wait by itself
	limitMaxAttempts  = 6                  // continues that hit the limit again before it gives up
	limitBackoffFirst = 5 * time.Minute    // the wait when the provider gave no reset time, doubling each try
	limitBackoffMax   = time.Hour
	limitMinWait      = 30 * time.Second // never closer than this: clocks differ, and a reset is not exact
	limitStagger      = 3 * time.Second  // between continues waiting on the same reset, so they don't all start at once
	limitMessageMax   = 300
	continueText      = "[The usage limit has reset. Continue what you were doing.]"
)

// limitStrong are phrases that only a spent subscription says; limitWeak could also be a passing
// per-minute throttle, so they count only when the provider's own numbers or the message show a reset.
var (
	limitStrong = regexp.MustCompile(`(?i)usage limit|(?:opus|sonnet|fable|haiku|session|weekly|daily|monthly|5-hour) limit reached|(?:hit|reached|exceeded) your (?:\w+[ -])?limit|(?:5-hour|five-hour|weekly|daily|monthly|session) limit|out of (?:usage|credits|messages)|free (?:usage|plan) (?:limit|exceeded)`)
	// limitNot are limits that are not the subscription running out (the Claude CLI words several this
	// way): a context window, concurrency, a budget set by the person, the usage page being throttled.
	limitNot  = regexp.MustCompile(`(?i)not your usage limit|context (?:window |length )?limit|concurren|budget limit|fast limit|subagent limit|device limit|token limit|size limit|character limit|max(?:imum)? (?:output )?tokens`)
	limitWeak = regexp.MustCompile(`(?i)rate.?limit|quota|too many requests`)
)

var (
	reEpoch = regexp.MustCompile(`\|(\d{9,10})\b`)
	reRFC   = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d(?::\d\d(?:\.\d+)?)?(?:Z|[+-]\d\d:\d\d)`)
	reIn    = regexp.MustCompile(`(?i)\b(?:in|after)\s+((?:\d+\s*(?:days?|d|hours?|hrs?|h|minutes?|mins?|m)\b\s*(?:and\s+|,\s*)?)+)`)
	reUnit  = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m)\b`)
	reClock = regexp.MustCompile(`(?i)(?:resets?|try again|available again|renews?)\s*(?:at|on|around)?\s*(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// reRetrySecs is a wait given in bare seconds, the way opencode's free-plan limit words it:
	// "Retry in ~20005s - attempt 1".
	reRetrySecs = regexp.MustCompile(`(?i)retry(?:ing)?\s+(?:in|after)\s*~?\s*(\d+)\s*s(?:ec(?:onds?)?)?\b`)
)

// parseLimitReset reads the time a limit message says it ends at: Claude's "|<unix seconds>", an RFC
// 3339 time, "in 2 hours 5 minutes", or a clock time such as "resets 3:05pm" (the next one, in loc).
func parseLimitReset(msg string, now time.Time, loc *time.Location) (time.Time, bool) {
	if m := reEpoch.FindStringSubmatch(msg); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(sec, 0), true
		}
	}
	if m := reRFC.FindString(msg); m != "" {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
			if t, err := time.Parse(layout, m); err == nil {
				return t, true
			}
		}
	}
	if m := reRetrySecs.FindStringSubmatch(msg); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil && sec > 0 {
			// Past the longest wait the caller marks it too far; the clamp only keeps an
			// absurd figure from overflowing the Duration.
			d := time.Duration(min(sec, int64(limitMaxWait/time.Second)+1)) * time.Second
			return now.Add(d), true
		}
	}
	if m := reIn.FindStringSubmatch(msg); m != nil {
		var d time.Duration
		for _, u := range reUnit.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(u[1])
			switch strings.ToLower(u[2])[0] {
			case 'd':
				d += time.Duration(n) * 24 * time.Hour
			case 'h':
				d += time.Duration(n) * time.Hour
			default:
				d += time.Duration(n) * time.Minute
			}
		}
		if d > 0 {
			return now.Add(d), true
		}
	}
	if m := reClock.FindStringSubmatch(msg); m != nil && (m[2] != "" || m[3] != "") {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		switch ap := strings.ToLower(m[3]); {
		case ap == "pm" && h < 12:
			h += 12
		case ap == "am" && h == 12:
			h = 0
		}
		if h < 24 && min < 60 {
			t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, loc)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// windowReset is when every spent window of rl is over (the latest reset among them), or false when
// none is spent or the numbers are out of date (the reset already passed).
func windowReset(rl protocol.ProviderRateLimits, now time.Time) (time.Time, bool) {
	return latestReset(rl.Windows(), now)
}

// planReset is windowReset for the plan-wide 5-hour and 7-day windows only: a spent per-model weekly
// bucket doesn't stop a chat that runs another model.
func planReset(rl protocol.ProviderRateLimits, now time.Time) (time.Time, bool) {
	var ws []protocol.OtherRateLimitWindow
	for _, w := range rl.Windows() {
		if w.Label == "" {
			ws = append(ws, w)
		}
	}
	return latestReset(ws, now)
}

func latestReset(windows []protocol.OtherRateLimitWindow, now time.Time) (time.Time, bool) {
	var latest time.Time
	for _, w := range windows {
		if w.UsedPct < limitFull {
			continue
		}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && t.After(now) && t.After(latest) {
			latest = t
		}
	}
	return latest, !latest.IsZero()
}

// limitBackoff is the guess for a limit whose end nobody said, doubling with each continue that hit it again.
func limitBackoff(attempt int) time.Duration {
	d := limitBackoffFirst
	for i := 0; i < attempt && d < limitBackoffMax; i++ {
		d *= 2
	}
	return min(d, limitBackoffMax)
}

// classifyLimit decides whether err is a usage limit and when it ends. rl are the provider's own
// numbers (zero value when unknown). estimated means nobody said, so the caller guesses.
func classifyLimit(err error, rl protocol.ProviderRateLimits, now time.Time, loc *time.Location) (resetsAt time.Time, estimated, ok bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// A provider that knows it is a usage limit says so itself (an ACP agent that
	// went silent), with no reset to read.
	var typed interface{ UsageLimit() bool }
	if errors.As(err, &typed) && typed.UsageLimit() {
		return time.Time{}, true, true
	}
	msg := err.Error()
	if limitNot.MatchString(msg) {
		return
	}
	strong, weak := limitStrong.MatchString(msg), limitWeak.MatchString(msg)
	if !strong && !weak {
		// A CLI can fail without saying why after its own "rejected" event; a turn that fails while the
		// plan's window is spent and not yet reset is the limit.
		if t, found := planReset(rl, now); found {
			return t, false, true
		}
		return
	}
	if t, found := windowReset(rl, now); found {
		return t, false, true
	}
	if t, found := parseLimitReset(msg, now, loc); found && t.After(now) {
		return t, false, true
	}
	return time.Time{}, true, strong
}

// onLimitHit is called with the error of a turn that failed; it reports whether it was a usage limit
// (and so handled: the caller shouldn't also report it as a plain turn error). origin is the origin of
// the turn that failed, so a continue that hit the limit again counts its attempts.
func onLimitHit(deps connDeps, pc *ProjectContext, sessionID, providerName string, err error, origin *protocol.Origin) bool {
	if deps.sched == nil || err == nil {
		return false
	}
	now := time.Now()
	var rl protocol.ProviderRateLimits
	if deps.status != nil {
		rl = deps.status.rateLimits()[providerName]
	}
	reset, estimated, ok := classifyLimit(err, rl, now, time.Local)
	if !ok {
		return false
	}
	attempt, wasAuto := 0, false
	if origin != nil && origin.Label == protocol.OriginLabelLimit {
		wasAuto = true
		if t, found := deps.sched.Get(origin.ScheduleID); found {
			attempt = t.Attempt + 1
		}
	}
	if estimated {
		reset = now.Add(limitBackoff(attempt))
	}
	if reset.Sub(now) < limitMinWait {
		reset = now.Add(limitMinWait)
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > limitMessageMax {
		msg = msg[:limitMessageMax] + "…"
	}
	hit := protocol.LimitHit{
		SessionID: sessionID, ProjectPath: pc.Root, Provider: providerName, AtMs: now.UnixMilli(),
		ResetsAtMs: reset.UnixMilli(), Estimated: estimated, Attempt: attempt, Message: msg,
		TooFar: reset.Sub(now) > limitMaxWait, GaveUp: attempt >= limitMaxAttempts,
	}
	// A continue that was already running keeps going, whatever the setting is now: the person
	// chose it for this chat.
	auto := (deps.settings.GetLimitAction() == protocol.LimitActionContinue || wasAuto) && !hit.TooFar && !hit.GaveUp
	deps.sched.SetHit(hit, auto)
	if auto {
		if err := armContinue(deps, hit); err != nil {
			logError("limit: arming a continue for %s: %v", sessionID, err)
			auto = false
		}
		deps.sched.Touch() // after a continue already waiting (armContinue adds nothing) there is still the new hit to show
	}
	notifyLimit(deps, pc, hit, auto)
	return true
}

// armContinue schedules the continue for a chat's limit hit, once: a chat with one waiting keeps it.
func armContinue(deps connDeps, hit protocol.LimitHit) error {
	if _, waiting := deps.sched.AutoFor(hit.SessionID); waiting {
		return nil
	}
	at := time.UnixMilli(hit.ResetsAtMs).Add(time.Duration(deps.sched.CountAuto()) * limitStagger)
	_, err := deps.sched.Add(protocol.ScheduleTurnData{
		SessionID: hit.SessionID, ProjectPath: hit.ProjectPath, Text: continueText,
		Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerLimitReset, AtMs: at.UnixMilli()},
		Origin:  &protocol.Origin{Kind: protocol.OriginScheduled, Label: protocol.OriginLabelLimit, WaitedMs: max(0, at.UnixMilli()-hit.AtMs)},
		Attempt: hit.Attempt,
	})
	return err
}

func notifyLimit(deps connDeps, pc *ProjectContext, hit protocol.LimitHit, auto bool) {
	if deps.notify == nil {
		return
	}
	when := time.UnixMilli(hit.ResetsAtMs).Format("3:04 pm")
	if hit.Estimated {
		when += " (a guess)"
	}
	n := protocol.Notification{
		Level: "warning", Kind: "limit_hit", Title: "Usage limit reached",
		SessionID: hit.SessionID, ProjectPath: pc.Root, Priority: "normal",
	}
	label := sessionLabel(pc.SessStore, hit.SessionID)
	switch {
	case hit.GaveUp:
		n.Body = labelledBody(label, fmt.Sprintf("Still limited after %d tries; it stopped trying.", hit.Attempt))
		n.Priority = "urgent"
	case hit.TooFar:
		n.Body = labelledBody(label, "The limit resets in more than a week.")
		n.Priority = "urgent"
	case auto:
		n.Body = labelledBody(label, "Continuing by itself around "+when+".")
	case deps.settings.GetLimitAction() == protocol.LimitActionStop:
		n.Body = labelledBody(label, "Resets around "+when+".")
	default:
		n.Body = labelledBody(label, "Resets around "+when+". Choose whether to continue then.")
		n.Priority = "urgent"
	}
	deps.notify.Push("", n)
}

// turnFailed reports a failed turn: as a usage limit when it was one, else as a plain turn error.
func turnFailed(deps connDeps, pc *ProjectContext, sessionID, providerName string, err error, origin *protocol.Origin) {
	if onLimitHit(deps, pc, sessionID, providerName, err, origin) {
		return
	}
	deps.notify.turnFailed(pc, sessionID, err)
}

// rearmLimitHits runs at start-up: a continue is only a row in the schedule, so one lost in a crash
// (its turn was running) leaves a hit with nothing waiting. In "continue" mode it gets one again; if
// the limit reset while the server was down, it is due at once.
func rearmLimitHits(deps connDeps) {
	if deps.sched == nil || deps.settings.GetLimitAction() != protocol.LimitActionContinue {
		return
	}
	for _, h := range deps.sched.List("").LimitHits {
		if h.GaveUp || h.TooFar {
			continue
		}
		if err := armContinue(deps, h); err != nil {
			logError("limit: re-arming %s: %v", h.SessionID, err)
		}
	}
}

// handleLimitEnvelope answers limit_continue, limit_dismiss and set_limit_action; it reports whether
// env was one. The first two are allowed for a restricted paired device (they are like sending a
// message); the setting is owner-only because it isn't in remoteAllowed.
func handleLimitEnvelope(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) bool {
	switch env.Type {
	case protocol.EventLimitContinue, protocol.EventLimitDismiss, protocol.EventSetLimitAction:
	default:
		return false
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: msg})
	}
	if deps.sched == nil {
		fail("usage-limit handling isn't available on this server")
		return true
	}
	switch env.Type {
	case protocol.EventSetLimitAction:
		var d protocol.SetLimitActionData
		if err := json.Unmarshal(env.Data, &d); err != nil {
			fail("malformed set_limit_action payload")
			return true
		}
		if err := deps.settings.SetLimitAction(d.Action); err != nil {
			fail(err.Error())
			return true
		}
		deps.peers.BroadcastEvent("", protocol.EventLimitAction, "", protocol.LimitActionData{Action: d.Action})
		if d.Action == protocol.LimitActionContinue { // the chats already waiting on a choice
			rearmLimitHits(deps)
		}
	case protocol.EventLimitDismiss:
		var d protocol.LimitDismissData
		if err := json.Unmarshal(env.Data, &d); err != nil || d.SessionID == "" {
			fail("limit_dismiss needs a sessionId")
			return true
		}
		deps.sched.Resolve(d.SessionID)
	case protocol.EventLimitContinue:
		var d protocol.LimitContinueData
		if err := json.Unmarshal(env.Data, &d); err != nil || d.SessionID == "" {
			fail("limit_continue needs a sessionId")
			return true
		}
		hit, ok := deps.sched.Hit(d.SessionID)
		if !ok {
			fail("this chat isn't waiting on a usage limit")
			return true
		}
		now := time.Now()
		if hit.TooFar && time.UnixMilli(hit.ResetsAtMs).Sub(now) > limitMaxWait {
			fail("the limit resets in more than a week; it won't be waited out automatically")
			return true
		}
		if hit.GaveUp { // an explicit try-again starts the count over
			hit.GaveUp, hit.Attempt = false, 0
			if time.UnixMilli(hit.ResetsAtMs).Sub(now) < limitMinWait {
				hit.ResetsAtMs = now.Add(limitMinWait).UnixMilli()
			}
			deps.sched.SetHit(hit, true)
		}
		if err := armContinue(deps, hit); err != nil {
			fail(err.Error())
		}
	}
	return true
}

// clearLimitOnSend ends a chat's limit hit when the person writes in it themselves.
func clearLimitOnSend(deps connDeps, sessionID string) {
	if deps.sched != nil {
		deps.sched.Resolve(sessionID)
	}
}
