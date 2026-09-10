package protocol_test

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"yoakeland/internal/protocol"
)

func TestIPCStream_RoundTrip(t *testing.T) {
	pipeR, pipeW := io.Pipe()
	streamR := protocol.NewIPCStream(pipeR, io.Discard)
	streamW := protocol.NewIPCStream(io.NopCloser(&bytes.Buffer{}), pipeW)

	go func() {
		defer pipeW.Close()
		_ = streamW.SendEvent(protocol.EventUserPrompt, "session-1", "turn-1", "", protocol.UserPromptData{
			Prompt: "hello yoakeland",
		})
	}()

	env, err := streamR.ReadEnvelope()
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if env.Type != protocol.EventUserPrompt {
		t.Fatalf("unexpected type %s", env.Type)
	}
	if env.SessionID != "session-1" {
		t.Fatalf("unexpected sessionId %s", env.SessionID)
	}
	if env.TurnID != "turn-1" {
		t.Fatalf("unexpected turnId %s", env.TurnID)
	}

	var data protocol.UserPromptData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("failed unmarshaling data: %v", err)
	}
	if data.Prompt != "hello yoakeland" {
		t.Fatalf("unexpected prompt %s", data.Prompt)
	}
}
