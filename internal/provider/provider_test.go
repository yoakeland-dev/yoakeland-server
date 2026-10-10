package provider_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"yoakeland/internal/provider"
)

func TestReadSSEStream(t *testing.T) {
	sseData := "event: ping\ndata: {\"ok\":true}\n\nevent: delta\ndata: chunk 1\ndata: chunk 2\n\n"
	var events []provider.SSEEvent

	err := provider.ReadSSEStream(strings.NewReader(sseData), func(ev provider.SSEEvent) error {
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

// A read error after a partial event must come back as an error, not as a
// clean end of stream with the half-read event dispatched.
func TestReadSSEStreamPartialThenError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("event: delta\ndata: partial\n"), errReader{boom})

	err := provider.ReadSSEStream(r, func(ev provider.SSEEvent) error {
		t.Errorf("partial event dispatched: %+v", ev)
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected scanner error %v, got %v", boom, err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
