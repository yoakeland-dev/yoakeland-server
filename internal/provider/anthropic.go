package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type AnthropicProvider struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client

	// thinkingBudget is set via SetModelOptions (set_model_options on the
	// wire); 0 means extended thinking is off. atomic since it can be
	// written from a different goroutine than an in-flight StreamTurn reads
	// it from.
	thinkingBudget atomic.Int64
}

// SetModelOptions implements agent.ModelOptionsSetter. ReasoningEffort is
// ignored — Anthropic's lever for this is the thinking budget, not a
// separate effort string.
func (p *AnthropicProvider) SetModelOptions(opts agent.ModelOptions) {
	p.thinkingBudget.Store(int64(opts.ThinkingBudget))
}

func (p *AnthropicProvider) ModelName() string { return p.Model }

func NewAnthropic(apiKey, model string) *AnthropicProvider {
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}
	return &AnthropicProvider{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.anthropic.com/v1/messages",
		Client:  &http.Client{},
	}
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicContentBlock
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	MaxTokens int                `json:"max_tokens"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

const titleSystemPrompt = "Summarize the user's message as a short chat title (plain text, no quotes, at most 8 words)."

// GenerateTitle implements agent.TitleGenerator with a small, non-streaming
// request on the same model as the session, separate from any in-flight
// StreamTurn call.
func (p *AnthropicProvider) GenerateTitle(ctx context.Context, prompt string) (string, error) {
	reqBody := anthropicRequest{
		Model:     p.Model,
		System:    titleSystemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
		Stream:    false,
		MaxTokens: 32,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("anthropic error (%d): %s", resp.StatusCode, errResp.Error.Message)
	}

	var out struct {
		Content []anthropicContentBlock `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, block := range out.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic: no text content in title response")
}

func (p *AnthropicProvider) StreamTurn(
	ctx context.Context,
	history []agent.Message,
	tools []tool.ToolDefinition,
) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 64)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		// 1. Format messages
		var msgs []anthropicMessage
		for _, m := range history {
			switch m.Role {
			case "user":
				msgs = append(msgs, anthropicMessage{Role: "user", Content: m.Content})
			case "assistant":
				var blocks []anthropicContentBlock
				if m.Content != "" {
					blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					var inputMap map[string]any
					_ = json.Unmarshal(tc.Args, &inputMap)
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Name,
						Input: inputMap,
					})
				}
				msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
			case "tool":
				msgs = append(msgs, anthropicMessage{
					Role: "user",
					Content: []anthropicContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: m.ToolCallID,
							Content:   m.Content,
						},
					},
				})
			}
		}

		// 2. Format tools
		var anthroTools []anthropicTool
		for _, t := range tools {
			anthroTools = append(anthroTools, anthropicTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.Parameters,
			})
		}

		maxTokens := 8192
		var thinking *anthropicThinking
		if budget := int(p.thinkingBudget.Load()); budget > 0 {
			thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
			// max_tokens must exceed the thinking budget it's carved out of.
			if maxTokens <= budget {
				maxTokens = budget + 8192
			}
		}

		reqBody := anthropicRequest{
			Model:     p.Model,
			System:    DefaultSystemPrompt,
			Messages:  msgs,
			Tools:     anthroTools,
			Stream:    true,
			MaxTokens: maxTokens,
			Thinking:  thinking,
		}

		jsonBytes, err := json.Marshal(reqBody)
		if err != nil {
			eChan <- err
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(jsonBytes))
		if err != nil {
			eChan <- err
			return
		}

		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")

		resp, err := p.Client.Do(req)
		if err != nil {
			eChan <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var errResp struct {
				Error struct{ Message string } `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&errResp)
			eChan <- fmt.Errorf("anthropic error (%d): %s", resp.StatusCode, errResp.Error.Message)
			return
		}

		// Track streaming tool accumulation
		var currentToolCall *agent.ToolCall
		var currentArgsBuffer bytes.Buffer

		var usage protocol.UsageData
		var stopReason string

		if readErr := ReadSSESteam(resp.Body, func(ev SSEEvent) error {
			switch ev.Event {
			case "message_start":
				var msg struct {
					Message struct {
						Usage struct {
							InputTokens  int `json:"input_tokens"`
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &msg); err == nil {
					usage.InputTokens = msg.Message.Usage.InputTokens
					usage.OutputTokens = msg.Message.Usage.OutputTokens
				}

			case "message_delta":
				var d struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &d); err == nil {
					if d.Usage.OutputTokens > 0 {
						usage.OutputTokens = d.Usage.OutputTokens
					}
					if d.Delta.StopReason == "max_tokens" {
						stopReason = "length"
					}
				}

			case "error":
				var e struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &e)
				// A mid-stream error event (e.g. overloaded_error on a long,
				// busy request) otherwise falls through unhandled here and the
				// stream just ends — indistinguishable from a normal finish,
				// so the turn gets reported as a clean "stop" instead of the
				// failure it actually was.
				return fmt.Errorf("anthropic stream error (%s): %s", e.Error.Type, e.Error.Message)

			case "content_block_start":
				var block struct {
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &block); err == nil {
					if block.ContentBlock.Type == "tool_use" {
						currentArgsBuffer.Reset()
						currentToolCall = &agent.ToolCall{
							ID:   block.ContentBlock.ID,
							Name: block.ContentBlock.Name,
						}
					}
				}

			case "content_block_delta":
				var delta struct {
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &delta); err == nil {
					switch delta.Delta.Type {
					case "text_delta":
						cChan <- agent.ModelStreamChunk{
							Kind:  agent.StreamChunkDelta,
							Delta: delta.Delta.Text,
						}
					case "input_json_delta":
						currentArgsBuffer.WriteString(delta.Delta.PartialJSON)
					}
				}

			case "content_block_stop":
				if currentToolCall != nil {
					currentToolCall.Args = json.RawMessage(currentArgsBuffer.Bytes())
					cChan <- agent.ModelStreamChunk{
						Kind:     agent.StreamChunkToolCall,
						ToolCall: currentToolCall,
					}
					currentToolCall = nil
				}
			}
			return nil
		}); readErr != nil {
			// A stream that ended because it failed (a dropped connection, a
			// truncated body, our own request context being cancelled) must not be
			// reported as a clean end-of-stream: without this the caller can't tell
			// a finished answer from a broken one, and a partial answer gets saved
			// and shown as a complete turn. context.Canceled here is an ordinary
			// user cancel, which the agent already recognizes as such.
			eChan <- readErr
			return
		}

		if stopReason != "" {
			cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkFinish, FinishReason: stopReason}
		}

		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &usage}
	}()

	return cChan, eChan
}
