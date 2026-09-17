package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type OpenAICompatibleProvider struct {
	APIKey       string
	Model        string
	BaseURL      string
	Client       *http.Client
	ExtraHeaders map[string]string

	// IsOpenRouter gates OpenRouter's native server-side web_search tool
	// (see NewOpenRouter): plain OpenAI-compatible endpoints (api.openai.com,
	// LM Studio, Ollama, ...) don't support it.
	IsOpenRouter bool

	// reasoningEffort is set via SetModelOptions; empty means omit the field
	// (the model's own default). OpenRouter accepts the same
	// "reasoning_effort" request field for its reasoning-capable models, so
	// this one field serves both OpenAI and OpenRouter uses of this struct.
	reasoningEffort atomic.Value // string
}

// SetModelOptions implements agent.ModelOptionsSetter. ThinkingBudget is
// ignored — OpenAI/OpenRouter's chat-completions API takes an effort level,
// not a token budget.
func (p *OpenAICompatibleProvider) SetModelOptions(opts agent.ModelOptions) {
	p.reasoningEffort.Store(opts.ReasoningEffort)
}

func (p *OpenAICompatibleProvider) getReasoningEffort() string {
	v, _ := p.reasoningEffort.Load().(string)
	return v
}

func (p *OpenAICompatibleProvider) ModelName() string { return p.Model }

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
		APIKey:       apiKey,
		Model:        model,
		BaseURL:      "https://openrouter.ai/api/v1/chat/completions",
		Client:       &http.Client{},
		IsOpenRouter: true,
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

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIRequest struct {
	Model           string              `json:"model"`
	Messages        []openAIMessage     `json:"messages"`
	Tools           []any               `json:"tools,omitempty"`
	Stream          bool                `json:"stream"`
	StreamOptions   openAIStreamOptions `json:"stream_options"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`

	// MaxTokens defaults to defaultMaxTokens below rather than being left
	// unset: several OpenAI-compatible backends (DeepSeek's API included)
	// apply their own low default — around 4096 — when this is omitted,
	// which a reasoning model can burn through entirely on hidden
	// reasoning_content before writing any visible content, cutting the
	// response off with nothing to show for it.
	MaxTokens int `json:"max_tokens,omitempty"`
}

const defaultMaxTokens = 8192

// GenerateTitle implements agent.TitleGenerator with a small, non-streaming
// request on the same model as the session, separate from any in-flight
// StreamTurn call.
func (p *OpenAICompatibleProvider) GenerateTitle(ctx context.Context, prompt string) (string, error) {
	reqBody := openAIRequest{
		Model: p.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: titleSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range p.ExtraHeaders {
		req.Header.Set(k, v)
	}

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
		return "", fmt.Errorf("provider error (%d): %s", resp.StatusCode, errResp.Error.Message)
	}

	var out struct {
		Choices []struct {
			Message openAIMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("provider: no text content in title response")
	}
	return out.Choices[0].Message.Content, nil
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
		if p.IsOpenRouter && os.Getenv("OPENROUTER_DISABLE_WEB_SEARCH") == "" {
			// OpenRouter's server-side web_search tool: resolved entirely
			// on OpenRouter's side (like Anthropic's native tool below),
			// billed to the same OPENROUTER_API_KEY already in use.
			// https://openrouter.ai/docs/guides/features/server-tools/web-search
			openAITools = append(openAITools, map[string]any{"type": "openrouter:web_search"})
		}
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

		// Genuine OpenAI is left out of the default: its o1/o3/o4/gpt-5
		// reasoning-family models reject max_tokens outright (they require
		// max_completion_tokens instead), and this default exists for
		// DeepSeek/OpenRouter/local backends, not those.
		maxTokens := defaultMaxTokens
		if p.BaseURL == "https://api.openai.com/v1/chat/completions" {
			maxTokens = 0
		}

		reqBody := openAIRequest{
			Model:           p.Model,
			Messages:        msgs,
			Tools:           openAITools,
			Stream:          true,
			StreamOptions:   openAIStreamOptions{IncludeUsage: true},
			ReasoningEffort: p.getReasoningEffort(),
			MaxTokens:       maxTokens,
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
		var usage protocol.UsageData
		var finishReason string

		if readErr := ReadSSESteam(resp.Body, func(ev SSEEvent) error {
			if ev.Data == "[DONE]" {
				return nil
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string           `json:"content"`
						ToolCalls []openAIToolCall `json:"tool_calls"`
						// ReasoningContent is DeepSeek-reasoner's (and
						// similar reasoning models') chain-of-thought
						// field, streamed alongside/before Content.
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}

			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				return nil
			}

			// The usage-carrying final chunk (stream_options.include_usage)
			// has an empty choices array, so check it before the early return.
			if chunk.Usage != nil {
				usage.InputTokens = chunk.Usage.PromptTokens
				usage.OutputTokens = chunk.Usage.CompletionTokens
				usage.TotalTokens = chunk.Usage.TotalTokens
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
			if choice.Delta.ReasoningContent != "" {
				cChan <- agent.ModelStreamChunk{
					Kind:  agent.StreamChunkReasoning,
					Delta: choice.Delta.ReasoningContent,
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

			// "length" means the response was truncated at the token limit
			// before the model naturally finished — without this, that looks
			// identical to an ordinary "stop" to the rest of the turn loop.
			if choice.FinishReason != nil && *choice.FinishReason == "length" {
				finishReason = "length"
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

		if finishReason != "" {
			cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkFinish, FinishReason: finishReason}
		}

		if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
			cChan <- agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &usage}
		}
	}()

	return cChan, eChan
}
