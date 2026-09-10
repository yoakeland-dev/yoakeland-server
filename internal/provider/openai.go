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

type OpenAICompatibleProvider struct {
	APIKey       string
	Model        string
	BaseURL      string
	Client       *http.Client
	ExtraHeaders map[string]string
}

func NewOpenAI(apiKey, model string) *OpenAICompatibleProvider {
	if model == "" {
		model = "gpt-4o"
	}
	return &OpenAICompatibleProvider{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.openai.com/v1/chat/completions",
		Client:  &http.Client{},
	}
}

func NewOpenRouter(apiKey, model string) *OpenAICompatibleProvider {
	if model == "" {
		model = "anthropic/claude-3.5-sonnet"
	}
	return &OpenAICompatibleProvider{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://openrouter.ai/api/v1/chat/completions",
		Client:  &http.Client{},
		ExtraHeaders: map[string]string{
			"HTTP-Referer": "https://github.com/yoakeland/yoakeland",
			"X-Title":      "Yoakeland",
		},
	}
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []any           `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

func (p *OpenAICompatibleProvider) StreamTurn(
	ctx context.Context,
	history []agent.Message,
	tools []tool.ToolDefinition,
) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 64)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		// 1. Convert messages
		msgs := []openAIMessage{
			{Role: "system", Content: DefaultSystemPrompt},
		}

		for _, m := range history {
			switch m.Role {
			case "user":
				msgs = append(msgs, openAIMessage{Role: "user", Content: m.Content})
			case "assistant":
				var tcs []openAIToolCall
				for i, tc := range m.ToolCalls {
					call := openAIToolCall{
						Index: i,
						ID:    tc.ID,
						Type:  "function",
					}
					call.Function.Name = tc.Name
					call.Function.Arguments = string(tc.Args)
					tcs = append(tcs, call)
				}
				msgs = append(msgs, openAIMessage{
					Role:      "assistant",
					Content:   m.Content,
					ToolCalls: tcs,
				})
			case "tool":
				msgs = append(msgs, openAIMessage{
					Role:       "tool",
					Content:    m.Content,
					ToolCallID: m.ToolCallID,
				})
			}
		}

		// 2. Convert tools
		var openAITools []any
		for _, t := range tools {
			openAITools = append(openAITools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}

		reqBody := openAIRequest{
			Model:    p.Model,
			Messages: msgs,
			Tools:    openAITools,
			Stream:   true,
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

		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range p.ExtraHeaders {
			req.Header.Set(k, v)
		}

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
			eChan <- fmt.Errorf("provider error (%d): %s", resp.StatusCode, errResp.Error.Message)
			return
		}

		// Accumulator map for streaming tool arguments
		toolCallAccum := make(map[int]*agent.ToolCall)
		toolArgBuffers := make(map[int]*bytes.Buffer)

		_ = ReadSSESteam(resp.Body, func(ev SSEEvent) error {
			if ev.Data == "[DONE]" {
				return nil
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string           `json:"content"`
						ToolCalls []openAIToolCall `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}

			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				return nil
			}

			if len(chunk.Choices) == 0 {
				return nil
			}

			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				cChan <- agent.ModelStreamChunk{
					Kind:  agent.StreamChunkDelta,
					Delta: choice.Delta.Content,
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				if _, exists := toolCallAccum[idx]; !exists {
					toolCallAccum[idx] = &agent.ToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
					}
					toolArgBuffers[idx] = &bytes.Buffer{}
				}

				if tc.ID != "" {
					toolCallAccum[idx].ID = tc.ID
				}
				if tc.Function.Name != "" {
					toolCallAccum[idx].Name = tc.Function.Name
				}
				toolArgBuffers[idx].WriteString(tc.Function.Arguments)
			}

			// When turn finishes, emit accumulated tool calls
			if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
				for i := 0; i < len(toolCallAccum); i++ {
					tc := toolCallAccum[i]
					tc.Args = json.RawMessage(toolArgBuffers[i].Bytes())
					cChan <- agent.ModelStreamChunk{
						Kind:     agent.StreamChunkToolCall,
						ToolCall: tc,
					}
				}
			}

			return nil
		})
	}()

	return cChan, eChan
}
