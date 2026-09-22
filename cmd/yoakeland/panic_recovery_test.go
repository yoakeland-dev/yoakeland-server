package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"yoakeland/internal/protocol"
)

// TestRecoverTurnPanic_ReportsErrorInsteadOfCrashing guards the fix for
// "chat deletion sometimes fails, restart fixes it": before this, a panic
// anywhere in turn handling (async turn goroutines, or now every
// synchronous envelope too, via dispatchEnvelope) took the whole process
// down with it. recoverTurnPanic must turn that into an ordinary `error`
// event instead of letting it propagate.
func TestRecoverTurnPanic_ReportsErrorInsteadOfCrashing(t *testing.T) {
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)

	func() {
		defer recoverTurnPanic(ipc, "sess1", "turn1")
		panic("simulated bug")
	}()

	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatalf("expected an error event to have been written, got nothing")
	}
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("malformed output %q: %v", line, err)
	}
	if env.Type != protocol.EventError {
		t.Fatalf("expected an error event, got %q", env.Type)
	}
	if env.SessionID != "sess1" || env.TurnID != "turn1" {
		t.Fatalf("expected the panic's session/turn to be reported, got sessionId=%q turnId=%q", env.SessionID, env.TurnID)
	}
	var data protocol.ErrorData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("malformed error data: %v", err)
	}
	if !strings.Contains(data.Message, "simulated bug") {
		t.Fatalf("expected the panic value in the error message, got %q", data.Message)
	}
}
