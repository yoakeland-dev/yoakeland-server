package protocol

// Artifact events (SPEC.md §3.1 artifact_event): what a page the model wrote (an ```artifact
// fence, shown by the client in a sandboxed frame) tells the server.

const EventArtifactEvent EventType = "artifact_event"

// Artifact event kinds.
const (
	// ArtifactSubmit is the user acting in the page (a choice, a form). It starts a turn.
	ArtifactSubmit = "submit"
	// ArtifactLog and ArtifactError are console output and uncaught errors, kept for the model
	// to read with the artifact_logs tool.
	ArtifactLog   = "log"
	ArtifactError = "error"
)

// MaxArtifactData is the most a single artifact event may carry (the JSON text).
const MaxArtifactData = 64 * 1024

// ArtifactEventData is artifact_event. Data is whatever JSON the page sent.
type ArtifactEventData struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath,omitempty"`
	ArtifactID  string `json:"artifactId"`
	Kind        string `json:"kind"`
	Data        any    `json:"data,omitempty"`
	// MessageID is the assistant message the artifact was in, so the answer can be tied to it.
	MessageID string `json:"messageId,omitempty"`
}
