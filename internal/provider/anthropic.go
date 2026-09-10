package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

type AnthropicProvider struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

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

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	MaxTokens int                `json:"max_tokens"`
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

		reqBody := anthropicRequest{
			Model:     p.Model,
			System:    DefaultSystemPrompt,
			Messages:  msgs,
			Tools:     anthroTools,
			Stream:    true,
			MaxTokens: 8192,
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

		_ = ReadSSESteam(resp.Body, func(ev SSEEvent) error {
			switch ev.Event {
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
		})
	}()

	return cChan, eChan
}
