package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type StreamEventKind string

const (
	StreamChunkDelta    StreamEventKind = "delta"
	StreamChunkToolCall StreamEventKind = "tool_call"
)

type ModelStreamChunk struct {
	Kind     StreamEventKind
	Delta    string
	ToolCall *ToolCall
}

type ModelProvider interface {
	StreamTurn(ctx context.Context, history []Message) (<-chan ModelStreamChunk, <-chan error)
}

type Agent struct {
	stream         *protocol.IPCStream
	provider       ModelProvider
	tools          map[string]tool.Tool
	permEngine     *permission.Engine
	snapshotStore  snapshot.SnapshotStore
	sessionStore   SessionStore
	contextManager *ContextManager

	sessionID    string
	history      []Message
	mu           sync.Mutex
	permRequests map[string]chan bool
}

func NewAgent(
	stream *protocol.IPCStream,
	provider ModelProvider,
	tools []tool.Tool,
	engine *permission.Engine,
	snapStore snapshot.SnapshotStore,
	sessStore SessionStore,
	ctxMgr *ContextManager,
) *Agent {
	toolMap := make(map[string]tool.Tool)
	for _, t := range tools {
		toolMap[t.Name()] = t
	}
	if ctxMgr == nil {
		ctxMgr = NewContextManager(32000, 2000)
	}

	return &Agent{
		stream:         stream,
		provider:       provider,
		tools:          toolMap,
		permEngine:     engine,
		snapshotStore:  snapStore,
		sessionStore:   sessStore,
		contextManager: ctxMgr,
		history:        make([]Message, 0),
		permRequests:   make(map[string]chan bool),
	}
}

func (a *Agent) LoadSession(sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessionStore == nil {
		return nil
	}

	state, err := a.sessionStore.Load(sessionID)
	if err != nil {
		// New session
		a.sessionID = sessionID
		a.history = make([]Message, 0)
		return nil
	}

	a.sessionID = state.SessionID
	a.history = state.History
	return nil
}

func (a *Agent) CurrentSessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

func (a *Agent) HandlePermissionResponse(toolCallID string, allowed bool, remember bool, cacheKey string) {
	a.mu.Lock()
	ch, exists := a.permRequests[toolCallID]
	delete(a.permRequests, toolCallID)
	a.mu.Unlock()

	if cacheKey != "" {
		a.permEngine.RecordDecision(cacheKey, allowed, remember)
	}

	if exists {
		ch <- allowed
	}
}

func (a *Agent) RunTurn(ctx context.Context, sessionID, turnID, prompt string) error {
	a.mu.Lock()
	if a.sessionID == "" || a.sessionID != sessionID {
		a.sessionID = sessionID
	}
	a.history = append(a.history, Message{Role: "user", Content: prompt})
	a.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		a.mu.Lock()
		activeContext := a.contextManager.PrepareHistory(a.history)
		a.mu.Unlock()

		chunks, errs := a.provider.StreamTurn(ctx, activeContext)

		var assistantText string
		var toolCalls []ToolCall

		done := false
		for !done {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err, ok := <-errs:
				if ok && err != nil {
					return fmt.Errorf("model stream error: %w", err)
				}
			case chunk, ok := <-chunks:
				if !ok {
					done = true
					break
				}
				if chunk.Kind == StreamChunkDelta {
					assistantText += chunk.Delta
					_ = a.stream.SendEvent(protocol.EventModelDelta, a.sessionID, turnID, "", protocol.ModelDeltaData{
						Content: chunk.Delta,
					})
				} else if chunk.Kind == StreamChunkToolCall && chunk.ToolCall != nil {
					toolCalls = append(toolCalls, *chunk.ToolCall)
					_ = a.stream.SendEvent(protocol.EventToolCallRequested, a.sessionID, turnID, chunk.ToolCall.ID, protocol.ToolCallData{
						Name: chunk.ToolCall.Name,
						Args: chunk.ToolCall.Args,
					})
				}
			}
		}

		a.mu.Lock()
		a.history = append(a.history, Message{
			Role:      "assistant",
			Content:   assistantText,
			ToolCalls: toolCalls,
		})
		a.mu.Unlock()

		if len(toolCalls) == 0 {
			break
		}

		// Execute tools
		for _, tc := range toolCalls {
			toolImpl, exists := a.tools[tc.Name]
			if !exists {
				a.recordToolResult(turnID, tc, "", fmt.Sprintf("tool %s not recognized", tc.Name))
				continue
			}

			allowed := true
			var cacheKey string
			if toolImpl.RequiresPermission(tc.Args) {
				var target string
				if tc.Name == "bash" {
					var bArgs struct{ Command string }
					_ = json.Unmarshal(tc.Args, &bArgs)
					target = bArgs.Command
				} else {
					var fArgs struct{ Path string }
					_ = json.Unmarshal(tc.Args, &fArgs)
					target = fArgs.Path
				}

				var decision permission.Decision
				decision, cacheKey = a.permEngine.Evaluate(tc.Name, target)

				switch decision {
				case permission.DecisionDeny:
					allowed = false
				case permission.DecisionAsk:
					respCh := make(chan bool, 1)
					a.mu.Lock()
					a.permRequests[tc.ID] = respCh
					a.mu.Unlock()

					_ = a.stream.SendEvent(protocol.EventPermissionRequest, a.sessionID, turnID, tc.ID, protocol.PermissionRequestData{
						ToolName:    tc.Name,
						Description: fmt.Sprintf("%s on %s", tc.Name, string(tc.Args)),
						RequiresAsk: true,
						CacheKey:    cacheKey,
					})

					select {
					case <-ctx.Done():
						return ctx.Err()
					case isAllowed := <-respCh:
						allowed = isAllowed
					}
				case permission.DecisionAllow:
					allowed = true
				}
			}

			if !allowed {
				a.recordToolResult(turnID, tc, "", "permission denied by user or policy")
				continue
			}

			execCtx := tool.ExecutionContext{
				SessionID:     a.sessionID,
				TurnID:        turnID,
				ToolCallID:    tc.ID,
				SnapshotStore: a.snapshotStore,
			}

			output, execErr := toolImpl.Execute(ctx, execCtx, tc.Args)
			truncatedOutput := a.contextManager.TruncateToolOutput(output)

			if execErr != nil {
				a.recordToolResult(turnID, tc, "", execErr.Error())
			} else {
				a.recordToolResult(turnID, tc, truncatedOutput, "")
			}
		}
	}

	// Persist session state to disk at the end of each turn
	a.mu.Lock()
	if a.sessionStore != nil && a.sessionID != "" {
		_ = a.sessionStore.Save(&SessionState{
			SessionID: a.sessionID,
			CreatedAt: time.Now().UTC(),
			History:   a.history,
		})
	}
	a.mu.Unlock()

	_ = a.stream.SendEvent(protocol.EventTurnCompleted, a.sessionID, turnID, "", protocol.TurnCompletedData{
		FinishReason: "stop",
		SessionID:    a.sessionID,
	})
	return nil
}

func (a *Agent) recordToolResult(turnID string, tc ToolCall, output, errText string) {
	a.mu.Lock()
	a.history = append(a.history, Message{
		Role:       "tool",
		Content:    output + errText,
		ToolCallID: tc.ID,
	})
	sessID := a.sessionID
	a.mu.Unlock()

	_ = a.stream.SendEvent(protocol.EventToolResult, sessID, turnID, tc.ID, protocol.ToolResultData{
		ToolName: tc.Name,
		Output:   output,
		Error:    errText,
	})
}
