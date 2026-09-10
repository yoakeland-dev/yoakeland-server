package tool

import (
	"context"
	"yoakeland/internal/snapshot"
)

type ExecutionContext struct {
	SessionID     string
	TurnID        string
	ToolCallID    string
	SnapshotStore snapshot.SnapshotStore
}

type Tool interface {
	Name() string
	Description() string
	RequiresPermission(argsJSON []byte) bool
	Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error)
}
