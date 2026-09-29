package main

import (
	"testing"
	"yoakeland/internal/protocol"
)

// approve_plan moves the session to the mode (and, with no agent waiting on
// the plan, asks it to carry the plan out: with no provider here that turn
// just fails, which the live plan test covers). Plan and unknown modes are
// rejected.
func TestApprovePlan(t *testing.T) {
	deps := newTestDeps(t)
	envs := runDispatchWaiting(t, deps,
		envelopeJSONWithSession(t, protocol.EventApprovePlan, "sess-plan", protocol.ApprovePlanData{Mode: "accept_edits"}),
		envelopeJSONWithSession(t, protocol.EventApprovePlan, "sess-plan", protocol.ApprovePlanData{Mode: "plan"}),
		envelopeJSONWithSession(t, protocol.EventApprovePlan, "sess-plan", protocol.ApprovePlanData{Mode: "yolo"}),
	)
	var modes, errs int
	for _, e := range envs {
		switch e.Type {
		case protocol.EventModeSet:
			modes++
		case protocol.EventError:
			errs++
		}
	}
	if modes != 1 || errs < 2 {
		t.Fatalf("mode_set %d, errors %d: %+v", modes, errs, envs)
	}
}
