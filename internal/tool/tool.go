package tool

import (
	"context"
	"yoakeland/internal/diff"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
)

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Question is one question for the user; no Options means free-form.
type Question struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

type ExecutionContext struct {
	SessionID     string
	TurnID        string
	ToolCallID    string
	SnapshotStore snapshot.SnapshotStore

	// Stream, if non-nil, lets a tool emit its own wire events (currently
	// just snapshot_capturing/snapshot_created/error around a pre-mutation
	// snapshot capture) to every connection currently subscribed to this
	// session — the same delivery path model_delta/tool_call_requested/etc.
	// already use (see protocol.StreamSet). nil in contexts that don't care
	// (most tests).
	Stream protocol.EventSender

	// AuthorName/AuthorEmail identify which peer's prompt started this
	// turn, if known (see agent.Author) — the bash tool uses these to set
	// GIT_AUTHOR_NAME/EMAIL so a commit the agent makes carries the human
	// who asked for it, not an anonymous/ambient identity. Empty when
	// unknown (e.g. a unix-socket peer with no registered token), in which
	// case nothing is injected and today's ambient git config applies.
	AuthorName  string
	AuthorEmail string

	// AskUsers, if non-nil, lets a tool (ask_user_question) pause the turn to
	// solicit input from the user and block until they have answered every
	// question (one answer each, in order) or the turn is cancelled. Injected
	// by Agent per dispatch — internal/tool never imports internal/agent, same
	// reasoning as Stream/AuthorName above.
	AskUsers func(qs []Question) ([]string, error)

	// ExtraReadPaths are files (or directories) the session's user attached
	// (agent.Message.Attachments): file_read may read these, and anything
	// under them, even though they're outside the project root.
	ExtraReadPaths []string
}

type Tool interface {
	Name() string
	Description() string
	Definition() ToolDefinition
	RequiresPermission(argsJSON []byte) bool
	Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error)
}

// DiffableTool allows tools like file_write and file_edit to generate a diff before applying changes
type DiffableTool interface {
	PreviewDiff(argsJSON []byte) (*diff.FileDiff, error)
}

// ReadOnlyTool is implemented by a tool that can say it changes nothing (an
// MCP tool's readOnlyHint). Only such tools outside the built-in set are
// offered in read_only and plan modes. The claim comes from the tool's
// server, so it gates what is offered, never what runs unasked.
type ReadOnlyTool interface {
	ReadOnly() bool
}

// Source supplies tools that come and go at runtime (MCP servers), on top of
// an agent's fixed set. Tools returns what is available now; Prepare starts
// whatever is not running yet and waits, bounded by ctx, for it to settle.
type Source interface {
	Tools() []Tool
	Prepare(ctx context.Context)
}
