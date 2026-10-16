package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"yoakeland/internal/audit"
)

// Limits are the server-wide soft limits on a turn, read live so a change
// reaches sessions that are already running. One *Limits is shared by every
// project's Registry (see Registry.SetLimits); the server's runtime settings
// write it (maxRoundsPerTurn, maxSessionCostUsd). Zero means unlimited. A nil
// *Limits is unlimited.
//
// They only bound yoakeland's own API-provider loop. A native CLI agent runs
// its own loop and answers for its own safety, so it is never limited here.
type Limits struct {
	mu         sync.RWMutex
	maxRounds  int
	maxCostUSD float64
}

func NewLimits(maxRounds int, maxCostUSD float64) *Limits {
	return &Limits{maxRounds: maxRounds, maxCostUSD: maxCostUSD}
}

func (l *Limits) Set(maxRounds int, maxCostUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxRounds, l.maxCostUSD = maxRounds, maxCostUSD
}

// Get returns the round ceiling and the per-session cost ceiling in USD.
func (l *Limits) Get() (maxRounds int, maxCostUSD float64) {
	if l == nil {
		return 0, 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.maxRounds, l.maxCostUSD
}

func (a *Agent) getLimits() *Limits {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limits
}

// isShellTool reports the tools that run arbitrary commands on the host: bash
// and the terminal_* family. Withheld together when the server's
// disableBashWhenUnsandboxed setting applies to a project.
func isShellTool(name string) bool {
	return name == "bash" || strings.HasPrefix(name, "terminal_")
}

// toolUnavailableMessage explains to the model why a tool it called is not
// allowed right now.
func (a *Agent) toolUnavailableMessage(name string) string {
	if a.ShellDisabled() && isShellTool(name) {
		return fmt.Sprintf("tool %s is disabled: this project has no sandbox, and the server withholds shell access without one", name)
	}
	return fmt.Sprintf("tool %s is unavailable in the current mode", name)
}

// turnLimits tracks one turn's progress towards the soft limits. A "Continue"
// answer raises the ceiling for this turn only, by one more allowance, so a
// long turn asks again after another N rounds or another cap's worth of spend
// instead of every round.
type turnLimits struct {
	rounds       int     // model round-trips started this turn
	extraRounds  int     // added by Continue answers
	extraCostUSD float64 // added by Continue answers
	pauses       int     // ids the questions apart
}

type limitKind int

const (
	limitNone limitKind = iota
	limitRounds
	limitCost
)

// limitHit says which soft limit the turn is at, with a sentence for the
// user. spentThisTurn is the cost priced so far this turn that the usage
// store has not recorded yet (unpriced usage counts as $0, as it does
// everywhere else).
func (a *Agent) limitHit(tl *turnLimits, spentThisTurn float64) (limitKind, string) {
	maxRounds, maxCost := a.getLimits().Get()
	if maxRounds > 0 && tl.rounds >= maxRounds+tl.extraRounds {
		return limitRounds, fmt.Sprintf("This turn has made %d model calls.", tl.rounds)
	}
	// Sub-agents have no usage store: they are checked against the tally
	// their round shares, which also counts the siblings running beside them.
	if maxCost > 0 && a.usageStore != nil {
		spent := a.usageStore.SessionTotal(a.CurrentSessionID()).CostUSD + spentThisTurn
		if spent >= maxCost+tl.extraCostUSD {
			return limitCost, fmt.Sprintf("This session has spent $%.2f, at its $%.2f limit.", spent, maxCost+tl.extraCostUSD)
		}
	}
	if maxCost > 0 && a.tally != nil {
		if spent := a.tally.total(); spent >= maxCost+tl.extraCostUSD {
			return limitCost, fmt.Sprintf("This session has spent $%.2f, at its $%.2f limit.", spent, maxCost+tl.extraCostUSD)
		}
	}
	return limitNone, ""
}

// pauseForLimit asks the user whether the turn should carry on, through the
// same question machinery ask_user_question uses: the question is a pending
// ask, surfaces in pending_asks, and blocks until answered or the turn is
// cancelled (err != nil). On "Continue" the ceiling that was hit is raised for
// this turn.
func (a *Agent) pauseForLimit(ctx context.Context, sessionID, turnID string, tl *turnLimits, kind limitKind, why string) (carryOn bool, err error) {
	tl.pauses++
	a.audit(audit.Event{Kind: audit.LimitPause, Outcome: "asked", Target: why, Detail: map[string]any{"turn": turnID}})
	answers, err := a.askUserQuestions(ctx, sessionID, turnID, []NativeQuestion{{
		ToolCallID: fmt.Sprintf("limit-%s-%d", turnID, tl.pauses),
		Question:   why + " Continue?",
		Options:    []string{"Continue", "Stop"},
	}})
	if err != nil {
		return false, err
	}
	if len(answers) == 0 || !strings.EqualFold(strings.TrimSpace(answers[0]), "continue") {
		return false, nil
	}
	maxRounds, maxCost := a.getLimits().Get()
	if kind == limitRounds {
		tl.extraRounds += maxRounds
	} else {
		tl.extraCostUSD += maxCost
	}
	return true, nil
}

// LimitReached is the finish reason of a turn the user stopped at a soft limit.
const LimitReached = "limit_reached"

// ProviderName is the registered name of the provider this session runs on,
// or "" when it has none.
func (a *Agent) ProviderName() string {
	if a.getProvider() == nil {
		return ""
	}
	return a.getProviderName()
}
