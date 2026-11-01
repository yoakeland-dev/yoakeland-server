package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sched"
)

// limitFull is the used share at which a rate-limit window counts as spent.
const limitFull = 99.5

// startScheduler wires the schedule to the rest of the server and starts its clock. A scheduled
// turn is an ordinary turn the server starts itself (SPEC.md §3.1 schedule_turn): it queues
// behind a running one, stops at permission prompts until a client answers, and is stamped with
// its origin so clients can say where it came from.
func startScheduler(ctx context.Context, deps connDeps) {
	if deps.sched == nil {
		return
	}
	// A turn with no client connected still needs a stream to write to; connected clients are
	// subscribed to the session's agent as usual.
	sink := protocol.NewIPCStream(strings.NewReader(""), io.Discard)
	deps.sched.Changed = func() {
		deps.peers.BroadcastEvent("", protocol.EventScheduleList, "", deps.sched.List(""))
	}
	deps.sched.Blocked = func(t protocol.ScheduledTurn) bool { return scheduleBlocked(deps, t) }
	deps.sched.Run = func(t protocol.ScheduledTurn, finished func()) error {
		pc, err := deps.projects.Resolve(t.SessionID, t.ProjectPath, deps.defaultProjectRoot)
		if err != nil {
			return err
		}
		ag := pc.Registry.Get(sink, t.SessionID)
		restoreBinding(deps, pc, ag, t.SessionID)
		if err := pc.SessStore.Ensure(t.SessionID); err != nil {
			logError("session %s: could not create its session file: %v", t.SessionID, err)
		}
		turnID := fmt.Sprintf("%s-r%d-%d", t.ID, t.Runs+1, time.Now().UnixMilli())
		turnCtx := agent.WithOrigin(agent.WithAuthor(ctx, agent.Author{}), t.Origin)
		if deps.turnWG != nil {
			deps.turnWG.Add(1)
		}
		go func() {
			if deps.turnWG != nil {
				defer deps.turnWG.Done()
			}
			defer finished()
			defer recoverTurnPanic(sink, t.SessionID, turnID)
			if err := ag.RunTurnWithTitleGeneration(turnCtx, t.SessionID, turnID, t.Text, false); err != nil {
				logError("scheduled turn %s (session %s) failed: %v", turnID, t.SessionID, err)
				turnFailed(deps, pc, t.SessionID, ag.ProviderName(), err, &t.Origin)
			} else if sched.IsAuto(t) {
				deps.sched.Resolve(t.SessionID) // the continue got through: the limit is over
			}
		}()
		deps.peers.BroadcastEvent("", protocol.EventScheduleFired, t.SessionID, protocol.ScheduleFiredData{ID: t.ID, TurnID: turnID})
		return nil
	}
	deps.sched.Start(ctx)
}

// scheduleBlocked reports whether a limit_reset entry's provider is out of quota.
func scheduleBlocked(deps connDeps, t protocol.ScheduledTurn) bool {
	name := t.Trigger.Provider
	if name == "" {
		if pc, err := deps.projects.Resolve(t.SessionID, t.ProjectPath, deps.defaultProjectRoot); err == nil {
			if ag := pc.Registry.Lookup(t.SessionID); ag != nil {
				name = ag.ProviderName()
			}
		}
		if b, ok := deps.sched.BindingFor(t.SessionID); ok && name == "" {
			name = b.Provider // the server restarted and no client has opened the chat yet
		}
	}
	if deps.status == nil {
		return false
	}
	rl, ok := deps.status.rateLimits()[name]
	return ok && limitsSpent(rl, time.Now())
}

// limitsSpent is true while any window of rl is full and hasn't reset yet.
func limitsSpent(rl protocol.ProviderRateLimits, now time.Time) bool {
	for _, w := range rl.Windows() {
		if w.UsedPct < limitFull {
			continue
		}
		reset, err := time.Parse(time.RFC3339, w.ResetsAt)
		if err != nil || reset.After(now) {
			return true
		}
	}
	return false
}

// handleScheduleEnvelope answers the schedule_* events; it reports whether env was one. They are
// not in remoteAllowed, so a restricted paired device never gets here.
func handleScheduleEnvelope(ipc *protocol.IPCStream, deps connDeps, env *protocol.Envelope) bool {
	switch env.Type {
	case protocol.EventScheduleTurn, protocol.EventListScheduled, protocol.EventCancelScheduled, protocol.EventPauseScheduled:
	default:
		return false
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: msg})
	}
	if deps.sched == nil {
		fail("scheduled turns aren't available on this server")
		return true
	}
	var err error
	switch env.Type {
	case protocol.EventScheduleTurn:
		var d protocol.ScheduleTurnData
		if err = json.Unmarshal(env.Data, &d); err != nil {
			fail("malformed schedule_turn payload")
			return true
		}
		if d.SessionID == "" {
			d.SessionID = env.SessionID
		}
		_, err = deps.sched.Add(d)
	case protocol.EventListScheduled:
		var d protocol.ListScheduledData
		_ = json.Unmarshal(env.Data, &d)
		_ = ipc.SendEvent(protocol.EventScheduleList, d.SessionID, env.TurnID, "", deps.sched.List(d.SessionID))
		return true
	case protocol.EventCancelScheduled:
		var d protocol.CancelScheduledData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			err = deps.sched.Cancel(d.ID)
		}
	case protocol.EventPauseScheduled:
		var d protocol.PauseScheduledData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			err = deps.sched.Pause(d.ID, d.All, d.Paused)
		}
	}
	if err != nil {
		fail(err.Error())
	}
	return true
}

// bindProvider builds the named provider for a chat's agent and remembers the choice, so a turn the
// server starts itself after a restart can use it again (restoreBinding).
func bindProvider(deps connDeps, pc *ProjectContext, ag *agent.Agent, sessionID, providerName, model string) error {
	cfg, ok := deps.providerStore.Get(providerName)
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}
	built, err := provider.BuildProvider(cfg, model, pc.Root)
	if err != nil {
		return fmt.Errorf("failed to activate provider %q: %v", providerName, err)
	}
	if cli, ok := built.(*provider.CLISubprocessProvider); ok && deps.status != nil {
		kind := string(cfg.Kind)
		cli.OnRateLimits = func(rl protocol.ProviderRateLimits) {
			rl.Kind = kind
			deps.status.SetRateLimits(providerName, rl)
		}
	}
	ag.SetProvider(built, providerName)
	if deps.sched != nil {
		deps.sched.RememberBinding(sessionID, sched.Binding{Provider: providerName, Model: model})
	}
	return nil
}

// restoreBinding gives a chat's agent the provider it last ran on when it has none (the server
// restarted and no client has opened the chat yet). A chat that never had one stays unbound, and its
// turn fails as no_provider, which is the truth.
func restoreBinding(deps connDeps, pc *ProjectContext, ag *agent.Agent, sessionID string) {
	if deps.sched == nil || deps.providerStore == nil || ag.ProviderName() != "" {
		return
	}
	if b, ok := deps.sched.BindingFor(sessionID); ok {
		if err := bindProvider(deps, pc, ag, sessionID, b.Provider, b.Model); err != nil {
			logError("chat %s: restoring its provider: %v", sessionID, err)
		}
	}
}
