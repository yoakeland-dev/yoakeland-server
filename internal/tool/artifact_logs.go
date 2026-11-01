package tool

import (
	"context"
	"encoding/json"

	"yoakeland/internal/artifact"
)

// ArtifactLogsToolName is how the model reads what an artifact page logged.
const ArtifactLogsToolName = "artifact_logs"

// ArtifactLogsTool returns the console output and errors of an artifact the model wrote, so it
// can see why its page misbehaves. It reads in-memory text only, so it needs no permission.
type ArtifactLogsTool struct{ logs *artifact.Logs }

func NewArtifactLogsTool(logs *artifact.Logs) *ArtifactLogsTool { return &ArtifactLogsTool{logs: logs} }

func (t *ArtifactLogsTool) Name() string { return ArtifactLogsToolName }

func (t *ArtifactLogsTool) Description() string {
	return "Read what an artifact page you wrote has logged (yoake.log, console) and the errors it threw. Use it when a page doesn't work. Without artifactId, the latest artifact of this chat."
}

func (t *ArtifactLogsTool) RequiresPermission(_ []byte) bool { return false }

func (t *ArtifactLogsTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var a struct {
		ArtifactID string `json:"artifactId"`
	}
	_ = json.Unmarshal(argsJSON, &a)
	return t.logs.Read(execCtx.SessionID, a.ArtifactID), nil
}

func (t *ArtifactLogsTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"artifactId": map[string]any{"type": "string", "description": "Optional: the artifact id shown in a submit message."},
			},
		},
	}
}
