package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"yoakeland/internal/protocol"
)

type WritePlanArgs struct {
	Content string `json:"content"`
}

// WritePlanTool is Plan mode's designated output channel: it records a
// structured plan to <plansDir>/<sessionId>.md (per session, so concurrent
// Plan-mode sessions don't clobber each other) and emits plan_written. Fire-and-forget — no
// blocking round trip, unlike ask_user_question. Exempt from the permission
// engine the same way file_read is: it's plan mode's own tool, allowlisted
// only there (see agent.planExtraTools), not a general-purpose mutation.
type WritePlanTool struct {
	plansDir string
}

func NewWritePlanTool(plansDir string) *WritePlanTool {
	return &WritePlanTool{plansDir: plansDir}
}

func (t *WritePlanTool) Name() string {
	return "write_plan"
}

func (t *WritePlanTool) Description() string {
	return "Record a structured plan for the user to review, opened in its own pane rather than only narrated in chat."
}

func (t *WritePlanTool) RequiresPermission(_ []byte) bool {
	return false
}

func (t *WritePlanTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args WritePlanArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if !protocol.ValidSessionID(execCtx.SessionID) {
		return "", fmt.Errorf("write_plan requires a valid session id")
	}

	if err := os.MkdirAll(t.plansDir, 0755); err != nil {
		return "", fmt.Errorf("creating plans directory: %w", err)
	}
	path := filepath.Join(t.plansDir, execCtx.SessionID+".md")
	if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("writing plan: %w", err)
	}

	if execCtx.Stream != nil {
		_ = execCtx.Stream.SendEvent(protocol.EventPlanWritten, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, protocol.PlanWrittenData{
			Path:    path,
			Content: args.Content,
		})
	}

	// No path: the model's file tools can't reach it anyway.
	return "plan recorded and shown to the user", nil
}

func (t *WritePlanTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "The plan's full content, in markdown.",
				},
			},
			"required": []string{"content"},
		},
	}
}
