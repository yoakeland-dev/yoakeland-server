package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AskUserQuestionArgs takes one or more questions; the single question/options
// pair is kept so an older call (or a model that prefers it) still works.
type AskUserQuestionArgs struct {
	Questions []Question `json:"questions,omitempty"`
	Question  string     `json:"question,omitempty"`
	Options   []string   `json:"options,omitempty"`
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
	return "Ask the user one or more clarifying questions and wait for their answers. Put every question you have in one call: they are shown together as a stack. Use when ambiguous instructions would otherwise force a guess."
}

func (t *AskUserQuestionTool) RequiresPermission(_ []byte) bool {
	return false
}

func (t *AskUserQuestionTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args AskUserQuestionArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	qs := args.Questions
	if args.Question != "" {
		qs = append([]Question{{args.Question, args.Options}}, qs...)
	}
	for _, q := range qs {
		if q.Question == "" {
			return "", fmt.Errorf("question must not be empty")
		}
	}
	if len(qs) == 0 {
		return "", fmt.Errorf("questions must not be empty")
	}
	if execCtx.AskUsers == nil {
		return "", fmt.Errorf("ask_user_question is not available in this context")
	}
	answers, err := execCtx.AskUsers(qs)
	if err != nil {
		return "", err
	}
	if len(qs) == 1 {
		return answers[0], nil
	}
	var b strings.Builder
	for i, q := range qs {
		fmt.Fprintf(&b, "Q: %s\nA: %s\n\n", q.Question, answers[i])
	}
	return strings.TrimSpace(b.String()), nil
}

func (t *AskUserQuestionTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{
					"type":        "array",
					"description": "The questions to ask, all at once.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"question": map[string]any{"type": "string", "description": "The question to ask the user."},
							"options": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Optional multiple-choice options for the user to pick from.",
							},
						},
						"required": []string{"question"},
					},
				},
			},
			"required": []string{"questions"},
		},
	}
}
