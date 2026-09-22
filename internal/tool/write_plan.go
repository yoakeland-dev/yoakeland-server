package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/protocol"
)

type WritePlanArgs struct {
	Content string `json:"content"`
}

// WritePlanTool is Plan mode's designated output channel: it records a
// structured plan to .yoake/plans/<sessionId>.md (per-session, so two
// concurrent Plan-mode sessions in one project never clobber each other)
// and emits plan_written so a frontend can open that file in a sidebar
// pane rather than relying only on model_delta prose. Fire-and-forget — no
// blocking round trip, unlike ask_user_question. Exempt from the permission
// engine the same way file_read is: it's plan mode's own tool, allowlisted
// only there (see agent.planExtraTools), not a general-purpose mutation.
type WritePlanTool struct {
	normalizer *normalizer.Normalizer
}

func NewWritePlanTool(norm *normalizer.Normalizer) *WritePlanTool {
	return &WritePlanTool{normalizer: norm}
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
	if execCtx.SessionID == "" {
		return "", fmt.Errorf("write_plan requires a session")
	}

	plansDir := filepath.Join(t.normalizer.ProjectRoot, ".yoake", "plans")
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		return "", fmt.Errorf("creating plans directory: %w", err)
	}

	relPath := filepath.Join(".yoake", "plans", execCtx.SessionID+".md")
	absPath := filepath.Join(t.normalizer.ProjectRoot, relPath)
	if err := os.WriteFile(absPath, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("writing plan: %w", err)
	}

	relPath = filepath.ToSlash(relPath)
	if execCtx.Stream != nil {
		_ = execCtx.Stream.SendEvent(protocol.EventPlanWritten, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, protocol.PlanWrittenData{
			Path: relPath,
		})
	}

	return fmt.Sprintf("plan written to %s", relPath), nil
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
