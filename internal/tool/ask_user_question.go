package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

type AskUserQuestionArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// AskUserQuestionTool pauses the turn and asks the user a clarifying
// question, blocking until they answer (see ExecutionContext.AskUser,
// injected by Agent — this is a separate, always-blocking mechanism from
// the permission_request/permission_response round trip, not routed
// through permission.Engine, and available regardless of mode since it's
// not a mutation.
type AskUserQuestionTool struct{}

func NewAskUserQuestionTool() *AskUserQuestionTool {
	return &AskUserQuestionTool{}
}

func (t *AskUserQuestionTool) Name() string {
	return "ask_user_question"
}

func (t *AskUserQuestionTool) Description() string {
	return "Ask the user a clarifying question and wait for their answer. Use when ambiguous instructions would otherwise force a guess."
}

func (t *AskUserQuestionTool) RequiresPermission(_ []byte) bool {
	return false
}

func (t *AskUserQuestionTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args AskUserQuestionArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Question == "" {
		return "", fmt.Errorf("question must not be empty")
	}
	if execCtx.AskUser == nil {
		return "", fmt.Errorf("ask_user_question is not available in this context")
	}
	return execCtx.AskUser(args.Question, args.Options)
}

func (t *AskUserQuestionTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask the user.",
				},
				"options": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional multiple-choice options for the user to pick from.",
				},
			},
			"required": []string{"question"},
		},
	}
}
