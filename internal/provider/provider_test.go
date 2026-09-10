package provider_test

import (
	"strings"
	"testing"
	"yoakeland/internal/provider"
)

func TestReadSSESteam(t *testing.T) {
	sseData := "event: ping\ndata: {\"ok\":true}\n\nevent: delta\ndata: chunk 1\ndata: chunk 2\n\n"
	var events []provider.SSEEvent

	err := provider.ReadSSESteam(strings.NewReader(sseData), func(ev provider.SSEEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Event != "ping" || events[0].Data != "{\"ok\":true}" {
		t.Errorf("unexpected event 0: %+v", events[0])
	}
	if events[1].Event != "delta" || events[1].Data != "chunk 1\nchunk 2" {
		t.Errorf("unexpected event 1: %+v", events[1])
	}
}
