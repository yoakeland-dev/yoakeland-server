package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
// not a token budget. An empty effort leaves the current one alone;
// "default" goes back to the model's own.
func (p *OpenAICompatibleProvider) SetModelOptions(opts agent.ModelOptions) {
	switch opts.ReasoningEffort {
	case "":
	case "default":
		p.reasoningEffort.Store("")
	default:
		p.reasoningEffort.Store(opts.ReasoningEffort)
	}
}

func (p *OpenAICompatibleProvider) getReasoningEffort() string {
	v, _ := p.reasoningEffort.Load().(string)
	return v
}

func (p *OpenAICompatibleProvider) ModelName() string { return p.Model }

// SupportsImages implements agent.VisionProvider: whether this model gets a
// picture inside a tool result, and so whether the browser tool offers its
// `screenshot` action at all (SPEC.md §4e).
func (p *OpenAICompatibleProvider) SupportsImages() bool { return SupportsImages(p.Model) }

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
	Role string `json:"role"`
	// Content is a string, or a []any of content parts ({type:"text"} /
	// {type:"image_url"}) when pictures ride with the message.
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []any           `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	// Pointer, not value: OpenAI 400s if stream_options is present at all
	// while stream is false (GenerateTitle's case) — a plain struct field
	// would always serialize since encoding/json's omitempty never treats a
	// non-pointer struct as empty.
	StreamOptions   *openAIStreamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`

	// PromptCacheKey (OpenAI only) routes a session's requests to the same
	// cache; CacheControl (OpenRouter, Claude models only) turns on its
	// automatic prompt caching — OpenAI and most others cache on their own.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	CacheControl   any    `json:"cache_control,omitempty"`

	// MaxTokens defaults to defaultMaxTokens below rather than being left
	// unset: several OpenAI-compatible backends (DeepSeek's API included)
	// apply their own low default — around 4096 — when this is omitted,
	// which a reasoning model can burn through entirely on hidden
	// reasoning_content before writing any visible content, cutting the
	// response off with nothing to show for it.
	MaxTokens int `json:"max_tokens,omitempty"`
}

const defaultMaxTokens = 8192

// post sends body to the provider, retrying transient failures (see
// doWithRetry).
func (p *OpenAICompatibleProvider) post(ctx context.Context, body []byte) (*http.Response, error) {
	return doWithRetry(ctx, p.Client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range p.ExtraHeaders {
			req.Header.Set(k, v)
		}
		return req, nil
	})
}

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

	resp, err := p.post(ctx, jsonBytes)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp, "provider")
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
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
	systemPrompt string,
) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 64)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		// 1. Convert messages
		msgs := []openAIMessage{
			{Role: "system", Content: systemPrompt},
		}

		// A tool result's pictures have no home in a `tool` message — the Chat
		// Completions shape keeps those text-only — so they follow their result
		// as one user message of image parts. Buffered, and flushed before the
		// next non-tool message (or at the end), so the run of tool messages
		// stays contiguous as servers expect.
		var pendingImages []protocol.ImageData
		flushImages := func() {
			if len(pendingImages) == 0 {
				return
			}
			parts := make([]any, 0, len(pendingImages)+1)
			parts = append(parts, map[string]any{"type": "text", "text": "Screenshot from the browser tool:"})
			for _, img := range pendingImages {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + img.MimeType + ";base64," + img.Data},
				})
			}
			msgs = append(msgs, openAIMessage{Role: "user", Content: parts})
			pendingImages = nil
		}

		for _, m := range withoutImages(history, p.SupportsImages()) {
			if m.Role != "tool" {
				flushImages()
			}
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
				pendingImages = append(pendingImages, m.Images...)
			}
		}
		flushImages()

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
			StreamOptions:   &openAIStreamOptions{IncludeUsage: true},
			ReasoningEffort: p.getReasoningEffort(),
			MaxTokens:       maxTokens,
		}
		// Only agent turns (they carry tools) are worth caching; a one-off
		// request would pay the write premium for nothing.
		if len(tools) > 0 {
			if p.BaseURL == "https://api.openai.com/v1/chat/completions" {
				reqBody.PromptCacheKey = agent.RequestHintsFrom(ctx).CacheKey
			}
			if p.IsOpenRouter && strings.HasPrefix(p.Model, "anthropic/") {
				reqBody.CacheControl = map[string]any{"type": "ephemeral"}
			}
		}

		jsonBytes, err := json.Marshal(reqBody)
		if err != nil {
			eChan <- err
			return
		}

		resp, err := p.post(ctx, jsonBytes)
		if err != nil {
			eChan <- err
			return
		}
		body := newStallReader(resp.Body, streamIdleTimeout)
		defer body.Close()

		if resp.StatusCode != http.StatusOK {
			eChan <- httpError(resp, "provider")
			return
		}

		// Tool calls are keyed by index, but a backend may skip indexes or send
		// none at all. Each call keeps its first-seen position, and a new id at
		// an index that already has a call starts a new one rather than merging
		// two calls' arguments.
		type toolAcc struct {
			call agent.ToolCall
			args bytes.Buffer
		}
		var calls []*toolAcc
		filling := make(map[int]*toolAcc) // index -> the call it is filling now
		var usage protocol.UsageData
		var finishReason string
		var sawData, sawFinish, sawDone bool
		send := func(c agent.ModelStreamChunk) error {
			select {
			case cChan <- c:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if readErr := ReadSSEStream(body, func(ev SSEEvent) error {
			if ev.Data == "[DONE]" {
				sawDone = true
				return nil
			}
			sawData = true

			var chunk struct {
				// A failure mid-stream arrives as a chunk with an error
				// member (OpenRouter sends it on HTTP 200).
				Error   json.RawMessage `json:"error"`
				Choices []struct {
					Delta struct {
						Content   string           `json:"content"`
						ToolCalls []openAIToolCall `json:"tool_calls"`
						// ReasoningContent is DeepSeek-reasoner's (and
						// similar reasoning models') chain-of-thought
						// field, streamed alongside/before Content.
						// Reasoning is OpenRouter's name for the same thing.
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens        int `json:"prompt_tokens"`
					CompletionTokens    int `json:"completion_tokens"`
					TotalTokens         int `json:"total_tokens"`
					PromptTokensDetails struct {
						CachedTokens     int `json:"cached_tokens"`
						CacheWriteTokens int `json:"cache_write_tokens"`
					} `json:"prompt_tokens_details"`
				} `json:"usage"`
			}

			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				return nil
			}
			if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
				msg := errorMessage(chunk.Error)
				if msg == "" {
					msg = string(chunk.Error)
				}
				return fmt.Errorf("provider stream error: %s", msg)
			}

			// The usage-carrying final chunk (stream_options.include_usage)
			// has an empty choices array, so check it before the early return.
			if chunk.Usage != nil {
				usage.InputTokens = chunk.Usage.PromptTokens
				usage.OutputTokens = chunk.Usage.CompletionTokens
				usage.TotalTokens = chunk.Usage.TotalTokens
				usage.CachedInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				usage.CacheWriteInputTokens = chunk.Usage.PromptTokensDetails.CacheWriteTokens
			}

			if len(chunk.Choices) == 0 {
				return nil
			}

			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: choice.Delta.Content}); err != nil {
					return err
				}
			}
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" {
				if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkReasoning, Delta: reasoning}); err != nil {
					return err
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				c := filling[tc.Index]
				if c == nil || (tc.ID != "" && c.call.ID != "" && tc.ID != c.call.ID) {
					c = &toolAcc{}
					calls = append(calls, c)
					filling[tc.Index] = c
				}
				if tc.ID != "" {
					c.call.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.call.Name = tc.Function.Name
				}
				c.args.WriteString(tc.Function.Arguments)
			}

			if fr := choice.FinishReason; fr != nil && *fr != "" {
				sawFinish = true
				switch *fr {
				case "error":
					return fmt.Errorf("provider stream error: the reply failed")
				// "length" means the response was truncated at the token limit
				// before the model naturally finished — without this, that looks
				// identical to an ordinary "stop" to the rest of the turn loop.
				case "length":
					finishReason = "length"
				// content_filter is the same thing as Anthropic's refusal.
				case "content_filter":
					finishReason = "refusal"
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

		// Some local servers end the stream without a [DONE] or a finish_reason.
		// That is only a clean end when nothing came before it.
		if !sawFinish && !sawDone && sawData {
			eChan <- fmt.Errorf("provider stream ended before the reply finished: %w", io.ErrUnexpectedEOF)
			return
		}

		// The tool calls go out once the stream is whole, so a backend that ends
		// the reply with "stop" instead of "tool_calls" still delivers them.
		for _, c := range calls {
			args := c.args.Bytes()
			if len(args) == 0 {
				args = []byte("{}")
			}
			c.call.Args = json.RawMessage(append([]byte(nil), args...))
			call := c.call
			if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkToolCall, ToolCall: &call}); err != nil {
				eChan <- err
				return
			}
		}

		if finishReason != "" {
			if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkFinish, FinishReason: finishReason}); err != nil {
				eChan <- err
				return
			}
		}

		if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
			if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &usage}); err != nil {
				eChan <- err
			}
		}
	}()

	return cChan, eChan
}
