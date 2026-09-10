package tool

import "context"

type ExecutionContext struct {
	TurnID     string
	ToolCallID string
}

type Tool interface {
	Name() string
	Description() string
	RequiresPermission(argsJSON []byte) bool
	Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error)
}
