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

type ExecutionContext struct {
	SessionID     string
	TurnID        string
	ToolCallID    string
	SnapshotStore snapshot.SnapshotStore

	// Stream, if non-nil, lets a tool emit its own wire events (currently
	// just snapshot_capturing/snapshot_created/error around a pre-mutation
	// snapshot capture) directly to whichever connection currently owns
	// this session — the same delivery path model_delta/tool_call_requested/
	// etc. already use. nil in contexts that don't care (most tests).
	Stream *protocol.IPCStream

	// AuthorName/AuthorEmail identify which peer's prompt started this
	// turn, if known (see agent.Author) — the bash tool uses these to set
	// GIT_AUTHOR_NAME/EMAIL so a commit the agent makes carries the human
	// who asked for it, not an anonymous/ambient identity. Empty when
	// unknown (e.g. a unix-socket peer with no registered token), in which
	// case nothing is injected and today's ambient git config applies.
	AuthorName  string
	AuthorEmail string
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
