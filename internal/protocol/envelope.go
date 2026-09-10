package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type EventType string

const (
	// Inbound (Client -> Backend)
	EventUserPrompt         EventType = "user_prompt"
	EventPermissionResponse EventType = "permission_response"
	EventCancelTurn         EventType = "cancel_turn"
	EventRestoreSnapshot    EventType = "restore_snapshot"

	// Outbound (Backend -> Client)
	EventModelDelta        EventType = "model_delta"
	EventToolCallRequested EventType = "tool_call_requested"
	EventPermissionRequest EventType = "permission_request"
	EventToolResult        EventType = "tool_result"
	EventTurnCompleted     EventType = "turn_completed"
	EventSnapshotCreated   EventType = "snapshot_created"
	EventSnapshotRestored  EventType = "snapshot_restored"
	EventSessionLoaded     EventType = "session_loaded"
	EventError             EventType = "error"
)

type Envelope struct {
	Type       EventType       `json:"type"`
	SessionID  string          `json:"sessionId,omitempty"`
	TurnID     string          `json:"turnId,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type UserPromptData struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"sessionId,omitempty"`
}

type ModelDeltaData struct {
	Content string `json:"content"`
}

type ToolCallData struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type PermissionRequestData struct {
	ToolName    string `json:"toolName"`
	Description string `json:"description"`
	RequiresAsk bool   `json:"requiresAsk"`
	CacheKey    string `json:"cacheKey"`
}

type PermissionResponseData struct {
	Allowed  bool `json:"allowed"`
	Remember bool `json:"remember"`
}

type ToolResultData struct {
	ToolName string `json:"toolName"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

type TurnCompletedData struct {
	FinishReason string `json:"finishReason"`
	SessionID    string `json:"sessionId"`
}

type SnapshotEventData struct {
	SnapshotID  string   `json:"snapshotId"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

type RestoreSnapshotData struct {
	SnapshotID string `json:"snapshotId"`
}

type SessionLoadedData struct {
	SessionID string `json:"sessionId"`
	TurnCount int    `json:"turnCount"`
}

type ErrorData struct {
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

type IPCStream struct {
	reader *bufio.Scanner
	writer io.Writer
	mu     sync.Mutex
}

func NewIPCStream(r io.Reader, w io.Writer) *IPCStream {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)
	return &IPCStream{
		reader: scanner,
		writer: w,
	}
}

func (s *IPCStream) ReadEnvelope() (*Envelope, error) {
	if !s.reader.Scan() {
		if err := s.reader.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	line := s.reader.Bytes()
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("malformed json line: %w", err)
	}
	return &env, nil
}

func (s *IPCStream) Send(env *Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}
	raw = append(raw, '\n')
	_, err = s.writer.Write(raw)
	return err
}

func (s *IPCStream) SendEvent(evtType EventType, sessionID, turnID, toolCallID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	return s.Send(&Envelope{
		Type:       evtType,
		SessionID:  sessionID,
		TurnID:     turnID,
		ToolCallID: toolCallID,
		Data:       raw,
	})
}
