package protocol

// Usage limits (SPEC.md §3.1 limit_continue): what the server does when a turn stops because the
// provider's subscription limit ran out.

const (
	EventLimitContinue  EventType = "limit_continue"
	EventLimitDismiss   EventType = "limit_dismiss"
	EventSetLimitAction EventType = "set_limit_action"
	EventLimitAction    EventType = "limit_action"
)

// What the server does when a turn hits a usage limit (the limit_action setting).
const (
	LimitActionAsk      = "ask"      // show a card; the person picks
	LimitActionContinue = "continue" // continue by itself once the limit resets
	LimitActionStop     = "stop"     // just report it
)

// OriginLabelLimit marks a scheduled turn the server armed to continue after a limit reset; a
// message the person sends meanwhile cancels it.
const OriginLabelLimit = "limit-continue"

// LimitHit is a chat whose last turn stopped at a usage limit and hasn't been picked up again. It
// is kept with the schedule (so it survives a restart and every client sees it) and sent in
// schedule_list.
type LimitHit struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	// Provider is the registered name of the provider that ran out.
	Provider string `json:"provider,omitempty"`
	AtMs     int64  `json:"atMs"`
	// ResetsAtMs is when the limit should be over. Estimated when the provider didn't say (a
	// guess that backs off each time a continue hits the limit again).
	ResetsAtMs int64 `json:"resetsAtMs"`
	Estimated  bool  `json:"estimated,omitempty"`
	// Attempt counts continues that hit the limit again; GaveUp is set when the server stopped trying.
	Attempt int  `json:"attempt,omitempty"`
	GaveUp  bool `json:"gaveUp,omitempty"`
	// TooFar is set when the reset is more than a week away, which the server won't wait out by itself.
	TooFar  bool   `json:"tooFar,omitempty"`
	Message string `json:"message,omitempty"`
}

// LimitContinueData is limit_continue: continue this chat by itself once its limit resets.
type LimitContinueData struct {
	SessionID string `json:"sessionId"`
}

// LimitDismissData is limit_dismiss: forget the limit hit, and any continue armed for it.
type LimitDismissData struct {
	SessionID string `json:"sessionId"`
}

// SetLimitActionData is set_limit_action; LimitActionData (limit_action) is it pushed to every peer
// and to a peer that connects.
type SetLimitActionData struct {
	Action string `json:"action"`
}
type LimitActionData struct {
	Action string `json:"action"`
}
