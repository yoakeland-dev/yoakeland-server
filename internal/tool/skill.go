package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SkillDoc is one skill's instructions, ready to hand the model.
type SkillDoc struct {
	Name      string
	Dir       string   // where the skill lives; its relative paths are relative to this
	Body      string   // SKILL.md without its frontmatter
	Files     []string // supporting files, relative to Dir
	Truncated bool
}

// SkillLoader finds a skill by name. byUser says the user asked for it (with
// $name) rather than the model: a skill its author marked user-only is
// refused to the model but not to them.
type SkillLoader interface {
	LoadSkill(name string, byUser bool) (SkillDoc, error)
}

// SkillToolName is how the model asks for a skill.
const SkillToolName = "skill"

// SkillTool loads a skill's instructions into the conversation: the second step
// of progressive disclosure. The system prompt names each skill and what it is
// for; this brings in the whole of one when the model decides it applies.
type SkillTool struct {
	loader SkillLoader
}

func NewSkillTool(l SkillLoader) *SkillTool { return &SkillTool{loader: l} }

func (t *SkillTool) Name() string { return SkillToolName }

func (t *SkillTool) Description() string {
	return "Load the full instructions of a skill listed under Skills in the system prompt, then follow them. " +
		"Give the skill's name exactly as listed. The result also names the skill's folder and the files in it: " +
		"read those with file_read when the instructions point to them."
}

func (t *SkillTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        SkillToolName,
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "The skill's name, as listed"},
			},
			"required": []string{"name"},
		},
	}
}

// RequiresPermission is false: loading a skill reads files the user put in a
// skill folder and changes nothing. What the instructions then ask the model to
// do goes through the permission checks like anything else it does.
func (t *SkillTool) RequiresPermission([]byte) bool { return false }

func (t *SkillTool) Execute(_ context.Context, _ ExecutionContext, argsJSON []byte) (string, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil || strings.TrimSpace(args.Name) == "" {
		return "", fmt.Errorf("give the name of the skill to load")
	}
	doc, err := t.loader.LoadSkill(strings.TrimPrefix(strings.TrimSpace(args.Name), "$"), false)
	if err != nil {
		return "", err
	}
	return FormatSkill(doc), nil
}

// FormatSkill is how a loaded skill reaches the model.
func FormatSkill(doc SkillDoc) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill name=%q>\n%s\n</skill>\n\nThis skill's folder is %s. Paths in its instructions are relative to it.", doc.Name, doc.Body, doc.Dir)
	if len(doc.Files) > 0 {
		sb.WriteString(" It also contains (read these with file_read when needed):\n")
		for _, f := range doc.Files {
			sb.WriteString("- " + f + "\n")
		}
	}
	return sb.String()
}
