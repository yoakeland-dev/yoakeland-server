package protocol

import "sync"

// EventSender is anything that can push an event to whoever's listening —
// a single connection's IPCStream, or a StreamSet fanning the same event
// out to several at once. agent.Agent and tool.ExecutionContext both hold
// one of these rather than a concrete *IPCStream, so a session with more
// than one connection subscribed to it (see StreamSet) needs no change at
// any of their call sites.
type EventSender interface {
	SendEvent(eventType EventType, sessionID, turnID, toolCallID string, payload any) error
}

// StreamSet fans an event out to every connection currently subscribed to
// it, instead of just one. This is what makes a session's turn stream
// (model_delta/tool_call_requested/permission_request/tool_result/
// turn_completed) reach every connection watching it — a second device, a
// second tab, a TCP peer alongside the local unix socket — rather than only
// whichever connection most recently touched that session, which used to
// silently cut every earlier viewer off (see SPEC.md's now-retired
// session_taken_over note).
type StreamSet struct {
	mu      sync.Mutex
	streams map[*IPCStream]struct{}
}

// NewStreamSet returns an empty set. Add at least one stream before sending
// anything through it — an empty set silently drops every event, the same
// as a nil *IPCStream would.
func NewStreamSet() *StreamSet {
	return &StreamSet{streams: make(map[*IPCStream]struct{})}
}

// Add subscribes stream, if it isn't already (keyed by pointer identity, so
// the same connection reconnecting to the same session twice is a no-op,
// not a double-send).
func (s *StreamSet) Add(stream *IPCStream) {
	if stream == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[stream] = struct{}{}
}

// Remove unsubscribes stream. Also called automatically by SendEvent on a
// write failure (see its doc comment) — callers don't have to hook
// connection teardown to keep this from accumulating dead entries, though
// doing so promptly (rather than waiting for the next event) is still
// preferable when a caller already knows a connection closed.
func (s *StreamSet) Remove(stream *IPCStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, stream)
}

// SendEvent delivers to every current subscriber. A subscriber whose write
// fails (a closed connection that hasn't been explicitly removed yet — e.g.
// its owning goroutine hasn't noticed the close) is dropped from the set
// right here, so a dead connection self-heals out of future broadcasts
// instead of accumulating forever. Returns the first error encountered, if
// any — callers already treat this the same as a single IPCStream.SendEvent
// error (logged or ignored, never fatal to the turn).
func (s *StreamSet) SendEvent(evtType EventType, sessionID, turnID, toolCallID string, payload any) error {
	s.mu.Lock()
	streams := make([]*IPCStream, 0, len(s.streams))
	for st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()

	var firstErr error
	for _, st := range streams {
		if err := st.SendEvent(evtType, sessionID, turnID, toolCallID, payload); err != nil {
			s.Remove(st)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
