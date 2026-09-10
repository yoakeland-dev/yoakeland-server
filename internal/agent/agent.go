package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// Message structures for LLM history
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

// ModelProvider allows swapping live LLMs (OpenAI/Anthropic) with deterministic mocks.
type ModelProvider interface {
	StreamTurn(ctx context.Context, history []Message) (<-chan ModelStreamChunk, <-chan error)
}

type Agent struct {
	stream       *protocol.IPCStream
	provider     ModelProvider
	tools        map[string]tool.Tool
	permEngine   *permission.Engine
	history      []Message
	mu           sync.Mutex
	permRequests map[string]chan bool // toolCallId -> response channel
}

func NewAgent(
	stream *protocol.IPCStream,
	provider ModelProvider,
	tools []tool.Tool,
	engine *permission.Engine,
) *Agent {
	toolMap := make(map[string]tool.Tool)
	for _, t := range tools {
		toolMap[t.Name()] = t
	}
	return &Agent{
		stream:       stream,
		provider:     provider,
		tools:        toolMap,
		permEngine:   engine,
		history:      make([]Message, 0),
		permRequests: make(map[string]chan bool),
	}
}

func (a *Agent) HandlePermissionResponse(toolCallID string, allowed bool, remember bool, cacheKey string) {
	a.mu.Lock()
	ch, exists := a.permRequests[toolCallID]
	delete(a.permRequests, toolCallID)
	a.mu.Unlock()

	if remember && cacheKey != "" {
		a.permEngine.RecordDecision(cacheKey, allowed)
	}

	if exists {
		ch <- allowed
	}
}

// RunTurn executes an iterative tool loop until the model yields without further tool requests.
func (a *Agent) RunTurn(ctx context.Context, turnID string, prompt string) error {
	a.history = append(a.history, Message{Role: "user", Content: prompt})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		chunks, errs := a.provider.StreamTurn(ctx, a.history)

		var assistantText string
		var toolCalls []ToolCall

		// Stream tokens from model
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
					_ = a.stream.SendEvent(protocol.EventModelDelta, turnID, "", protocol.ModelDeltaData{
						Content: chunk.Delta,
					})
				} else if chunk.Kind == StreamChunkToolCall && chunk.ToolCall != nil {
					toolCalls = append(toolCalls, *chunk.ToolCall)
					_ = a.stream.SendEvent(protocol.EventToolCallRequested, turnID, chunk.ToolCall.ID, protocol.ToolCallData{
						Name: chunk.ToolCall.Name,
						Args: chunk.ToolCall.Args,
					})
				}
			}
		}

		a.history = append(a.history, Message{
			Role:      "assistant",
			Content:   assistantText,
			ToolCalls: toolCalls,
		})

		// Exit agent loop if no tool calls were generated
		if len(toolCalls) == 0 {
			break
		}

		// Execute tools sequentially
		for _, tc := range toolCalls {
			toolImpl, exists := a.tools[tc.Name]
			if !exists {
				errMsg := fmt.Sprintf("tool %s not recognized", tc.Name)
				a.recordToolResult(turnID, tc, "", errMsg)
				continue
			}

			// Permission handling
			allowed := true
			if toolImpl.RequiresPermission(tc.Args) {
				var commandStr string
				// If tool is bash, inspect args for permission cache key
				if tc.Name == "bash" {
					var bArgs struct{ Command string }
					_ = json.Unmarshal(tc.Args, &bArgs)
					commandStr = bArgs.Command
				}

				decision, cacheKey := a.permEngine.Evaluate(tc.Name, commandStr)
				switch decision {
				case permission.DecisionDeny:
					allowed = false
				case permission.DecisionAsk:
					respCh := make(chan bool, 1)
					a.mu.Lock()
					a.permRequests[tc.ID] = respCh
					a.mu.Unlock()

					_ = a.stream.SendEvent(protocol.EventPermissionRequest, turnID, tc.ID, protocol.PermissionRequestData{
						ToolName:    tc.Name,
						Description: fmt.Sprintf("Run %s with args: %s", tc.Name, string(tc.Args)),
						RequiresAsk: true,
						CacheKey:    cacheKey,
					})

					// Wait for frontend response or context cancel
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

			// Tool Execution
			execCtx := tool.ExecutionContext{TurnID: turnID, ToolCallID: tc.ID}
			output, execErr := toolImpl.Execute(ctx, execCtx, tc.Args)
			if execErr != nil {
				a.recordToolResult(turnID, tc, "", execErr.Error())
			} else {
				a.recordToolResult(turnID, tc, output, "")
			}
		}
	}

	_ = a.stream.SendEvent(protocol.EventTurnCompleted, turnID, "", protocol.TurnCompletedData{
		FinishReason: "stop",
	})
	return nil
}

func (a *Agent) recordToolResult(turnID string, tc ToolCall, output, errText string) {
	a.history = append(a.history, Message{
		Role:       "tool",
		Content:    output + errText,
		ToolCallID: tc.ID,
	})

	_ = a.stream.SendEvent(protocol.EventToolResult, turnID, tc.ID, protocol.ToolResultData{
		ToolName: tc.Name,
		Output:   output,
		Error:    errText,
	})
}
