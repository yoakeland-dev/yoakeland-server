package protocol

// Scheduled turns (SPEC.md §3.1 schedule_turn): a turn the server starts itself, from an
// artifact submit, a timer, a repeat or the end of a rate limit, instead of a client's
// user_prompt. Every message such a turn adds carries an Origin so a client can say where it
// came from.

const (
	EventScheduleTurn    EventType = "schedule_turn"
	EventListScheduled   EventType = "list_scheduled"
	EventCancelScheduled EventType = "cancel_scheduled"
	EventPauseScheduled  EventType = "pause_scheduled"
	EventScheduleList    EventType = "schedule_list"
	EventScheduleFired   EventType = "schedule_fired"
)

// Origin kinds. A message with no origin was typed by a person.
const (
	OriginUser      = "user"
	OriginArtifact  = "artifact"
	OriginScheduled = "scheduled"
)

// Origin says who started a turn when it wasn't a person typing: an artifact the user
// interacted with, or a scheduled turn. Nil means a person.
type Origin struct {
	Kind       string `json:"kind"`
	ArtifactID string `json:"artifactId,omitempty"`
	Label      string `json:"label,omitempty"`
	ScheduleID string `json:"scheduleId,omitempty"`
	// Trigger is the scheduled turn's trigger kind: "now", "at", "limit_reset" or "repeat".
	Trigger string `json:"trigger,omitempty"`
	// WaitedMs is how long a limit-continue waited, from the limit hit to the reset.
	WaitedMs int64 `json:"waitedMs,omitempty"`
}

// Trigger kinds of a ScheduleTrigger.
const (
	TriggerNow        = "now"
	TriggerAt         = "at"
	TriggerLimitReset = "limit_reset"
	TriggerRepeat     = "repeat"
)

// ScheduleTrigger says when a scheduled turn runs.
type ScheduleTrigger struct {
	Kind string `json:"kind"`
	// AtMs (kind "at") is the Unix time in milliseconds.
	AtMs int64 `json:"atMs,omitempty"`
	// Provider (kind "limit_reset") names the provider whose limits hold the turn back; empty
	// means the session's own provider.
	Provider string `json:"provider,omitempty"`
	// EveryMs (kind "repeat") is the interval, at least a minute; MaxRuns caps how often it runs.
	EveryMs int64 `json:"everyMs,omitempty"`
	MaxRuns int   `json:"maxRuns,omitempty"`
}

// Schedule statuses.
const (
	SchedulePending   = "pending"
	ScheduleRunning   = "running"
	ScheduleDone      = "done"
	ScheduleCancelled = "cancelled"
	SchedulePaused    = "paused"
)

// ScheduledTurn is one entry of the server's schedule.
type ScheduledTurn struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"sessionId"`
	ProjectPath string          `json:"projectPath,omitempty"`
	Text        string          `json:"text"`
	Origin      Origin          `json:"origin"`
	Trigger     ScheduleTrigger `json:"trigger"`
	Status      string          `json:"status"`
	CreatedAtMs int64           `json:"createdAtMs"`
	// NextRunAtMs is when it is next due (0 when that depends on something other than time,
	// such as a limit resetting).
	NextRunAtMs int64 `json:"nextRunAtMs,omitempty"`
	Runs        int   `json:"runs"`
	// Attempt counts the limit hits behind a continue armed after a usage limit (see LimitHit).
	Attempt int `json:"attempt,omitempty"`
	// Error is why the last run didn't start, if it didn't.
	Error string `json:"error,omitempty"`
	// CreatorUserID and CreatorCredential say who armed it; it runs as them, with the access they
	// have when it is due. Both empty (an entry from before users) means the owner.
	CreatorUserID     string `json:"creatorUserId,omitempty"`
	CreatorCredential string `json:"creatorCredential,omitempty"`
}

// ScheduleTurnData is schedule_turn: arm a turn. Origin defaults to a plain scheduled one.
type ScheduleTurnData struct {
	SessionID   string          `json:"sessionId"`
	ProjectPath string          `json:"projectPath,omitempty"`
	Text        string          `json:"text"`
	Trigger     ScheduleTrigger `json:"trigger"`
	Origin      *Origin         `json:"origin,omitempty"`
	Attempt     int             `json:"attempt,omitempty"`
}

// ListScheduledData is list_scheduled; SessionID empty lists every session's.
type ListScheduledData struct {
	SessionID string `json:"sessionId,omitempty"`
}

// CancelScheduledData is cancel_scheduled.
type CancelScheduledData struct {
	ID string `json:"id"`
}

// PauseScheduledData is pause_scheduled: one entry by ID, or All of them.
type PauseScheduledData struct {
	ID     string `json:"id,omitempty"`
	All    bool   `json:"all,omitempty"`
	Paused bool   `json:"paused"`
}

// ScheduleListData is schedule_list: every schedule (the session's when SessionID is set),
// and whether the whole schedule is paused. It is also pushed to every peer when it changes.
type ScheduleListData struct {
	SessionID string          `json:"sessionId,omitempty"`
	Schedules []ScheduledTurn `json:"schedules"`
	AllPaused bool            `json:"allPaused,omitempty"`
	// LimitHits are the chats stopped at a usage limit (limit.go), all of them.
	LimitHits []LimitHit `json:"limitHits,omitempty"`
}

// ScheduleFiredData is schedule_fired: the server started TurnID for schedule ID.
type ScheduleFiredData struct {
	ID     string `json:"id"`
	TurnID string `json:"turnId"`
}
