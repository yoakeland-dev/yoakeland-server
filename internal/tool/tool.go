package tool

import (
	"context"
	"yoakeland/internal/diff"
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
