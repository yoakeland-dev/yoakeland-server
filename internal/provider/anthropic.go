package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
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

	// effort and thinkingBudget are set via SetModelOptions
	// (set_model_options on the wire). atomic since they can be written from
	// a different goroutine than an in-flight StreamTurn reads them from.
	effort         atomic.Value // string
	thinkingBudget atomic.Int64
}

// SetModelOptions implements agent.ModelOptionsSetter. Empty fields leave
// the current value alone; effort "default" clears both back to the
// model's own default.
func (p *AnthropicProvider) SetModelOptions(opts agent.ModelOptions) {
	switch opts.ReasoningEffort {
	case "":
	case "default":
		p.effort.Store("")
		p.thinkingBudget.Store(0)
	default:
		p.effort.Store(opts.ReasoningEffort)
	}
	if opts.ThinkingBudget != 0 {
		p.thinkingBudget.Store(int64(opts.ThinkingBudget))
	}
}

func (p *AnthropicProvider) getEffort() string {
	v, _ := p.effort.Load().(string)
	return v
}

func (p *AnthropicProvider) ModelName() string { return p.Model }

// SupportsImages implements agent.VisionProvider: whether this model gets a
// picture inside a tool result, and so whether the browser tool offers its
// `screenshot` action at all (SPEC.md §4e).
func (p *AnthropicProvider) SupportsImages() bool { return SupportsImages(p.Model) }

func NewAnthropic(apiKey, model string) *AnthropicProvider {
	if model == "" {
		model = "claude-sonnet-5"
	}
	return &AnthropicProvider{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.anthropic.com/v1/messages",
		Client:  &http.Client{},
	}
}

// legacyThinkingModel matches the models that predate adaptive thinking and
// the effort parameter (Claude 3.x, Haiku, and Opus/Sonnet 4 through 4.5):
// those take a fixed thinking budget instead, and reject effort.
var legacyThinkingModel = regexp.MustCompile(`^claude-(3|haiku|(opus|sonnet)-4(-[015])?(-\d{8})?$)`)

func adaptiveThinking(model string) bool { return !legacyThinkingModel.MatchString(model) }

// effortBudget maps an effort level onto a thinking budget, for the legacy
// models that only take a budget — so one effort picker works everywhere.
var effortBudget = map[string]int{"low": 2048, "medium": 8192, "high": 16384, "xhigh": 24576, "max": 32000}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &anthropicCacheControl{Type: "ephemeral"}

type anthropicTool struct {
	Type        string                 `json:"type,omitempty"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]any         `json:"input_schema,omitempty"`
	MaxUses     int                    `json:"max_uses,omitempty"`
	Model       string                 `json:"model,omitempty"`   // advisor tool only
	Caching     *anthropicCacheControl `json:"caching,omitempty"` // advisor tool only
}

// webSearchTool is Anthropic's server-side web search tool: Claude decides
// when to search, the API runs it and feeds results back into the same
// request, and yoakeland never sees a tool_use block for it — it's billed
// to the same ANTHROPIC_API_KEY already in use, no separate search API key
// needed. The newer version filters results before they reach the context
// (fewer input tokens) and needs Opus/Sonnet 4.6 or later. Set
// ANTHROPIC_DISABLE_WEB_SEARCH=1 for organizations that have disabled it in
// the Console: including the tool on such an account makes the API reject
// the whole request with a 400.
// https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool
func webSearchTool(model string) anthropicTool {
	t := anthropicTool{Type: "web_search_20250305", Name: "web_search", MaxUses: 5}
	if adaptiveThinking(model) && (strings.Contains(model, "opus") || strings.Contains(model, "sonnet")) {
		t.Type = "web_search_20260209"
	}
	return t
}

type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        map[string]any         `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"` // a string, or []any of *anthropicContentBlock for a tool result with pictures
	Source       *anthropicImageSource  `json:"source,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []any of *anthropicContentBlock / json.RawMessage
}

// anthropicImageSource is the base64 half of an image block: a screenshot the
// browser tool took, on its way to the model.
type anthropicImageSource struct {
	Type      string `json:"type"` // always "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// toolResultContent is one tool result as Anthropic takes it: the plain string
// for a text-only result (still the common case), or a block array when
// pictures ride along — a tool_result's content takes text and image blocks,
// which is how a screenshot reaches the model.
func toolResultContent(m *agent.Message) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	out := make([]any, 0, len(m.Images)+1)
	if m.Content != "" {
		out = append(out, &anthropicContentBlock{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		out = append(out, &anthropicContentBlock{Type: "image", Source: &anthropicImageSource{
			Type:      "base64",
			MediaType: img.MimeType,
			Data:      img.Data,
		}})
	}
	return out
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	System       any                    `json:"system,omitempty"` // string, or []*anthropicContentBlock
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Stream       bool                   `json:"stream"`
	MaxTokens    int                    `json:"max_tokens"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

// post sends a Messages API request, retrying transient failures (see
// doWithRetry). betas is the anthropic-beta header, when there is one.
func (p *AnthropicProvider) post(ctx context.Context, body []byte, betas []string) (*http.Response, error) {
	return doWithRetry(ctx, p.Client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		if len(betas) > 0 {
			req.Header.Set("anthropic-beta", strings.Join(betas, ","))
		}
		return req, nil
	})
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
	// Newer models may think before answering (some always do), which a
	// 32-token cap would cut off before any title.
	if adaptiveThinking(p.Model) {
		reqBody.MaxTokens = 1024
		reqBody.OutputConfig = &anthropicOutputConfig{Effort: "low"}
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	resp, err := p.post(ctx, jsonBytes, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp, "anthropic")
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

// anthropicMessages converts history to the Messages API shape: consecutive
// tool results (and anything else from the user side) go in one user
// message — split across several, Claude learns to stop calling tools in
// parallel — and an assistant message with RawContent is sent back
// verbatim, thinking blocks and all.
func anthropicMessages(history []agent.Message) []anthropicMessage {
	var msgs []anthropicMessage
	appendUser := func(block *anthropicContentBlock) {
		if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
			msgs[n-1].Content = append(msgs[n-1].Content.([]any), block)
			return
		}
		msgs = append(msgs, anthropicMessage{Role: "user", Content: []any{block}})
	}
	for _, m := range history {
		switch m.Role {
		case "user":
			appendUser(&anthropicContentBlock{Type: "text", Text: m.Content})
		case "assistant":
			var blocks []any
			var raw []json.RawMessage
			if len(m.RawContent) > 0 && json.Unmarshal(m.RawContent, &raw) == nil {
				for _, b := range raw {
					blocks = append(blocks, b)
				}
			} else {
				if m.Content != "" {
					blocks = append(blocks, &anthropicContentBlock{Type: "text", Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					inputMap := map[string]any{}
					_ = json.Unmarshal(tc.Args, &inputMap)
					blocks = append(blocks, &anthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: inputMap})
				}
			}
			msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
		case "tool":
			appendUser(&anthropicContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: toolResultContent(&m)})
		}
	}
	return msgs
}

// markLastBlock puts a cache breakpoint on the last content block of the
// conversation: the next request, which repeats everything up to here,
// reads it all back at the cache rate.
func markLastBlock(msgs []anthropicMessage) {
	if len(msgs) == 0 {
		return
	}
	blocks, _ := msgs[len(msgs)-1].Content.([]any)
	if len(blocks) == 0 {
		return
	}
	if b, ok := blocks[len(blocks)-1].(*anthropicContentBlock); ok {
		b.CacheControl = ephemeral
	}
}

// anthropicStreamBlocks rebuilds a response's content blocks from its
// stream, so blocks yoakeland has no field for (thinking and its
// signature, server tool calls and results) can be replayed verbatim.
type anthropicStreamBlocks struct {
	blocks []map[string]any
	inputs map[int]*bytes.Buffer
}

func (s *anthropicStreamBlocks) start(index int, block map[string]any) {
	for len(s.blocks) <= index {
		s.blocks = append(s.blocks, nil)
	}
	s.blocks[index] = block
}

func (s *anthropicStreamBlocks) delta(index int, d map[string]any) {
	if index >= len(s.blocks) || s.blocks[index] == nil {
		return
	}
	b := s.blocks[index]
	str := func(v any) string { s, _ := v.(string); return s }
	switch d["type"] {
	case "text_delta":
		b["text"] = str(b["text"]) + str(d["text"])
	case "thinking_delta":
		b["thinking"] = str(b["thinking"]) + str(d["thinking"])
	case "signature_delta":
		b["signature"] = d["signature"]
	case "citations_delta":
		cites, _ := b["citations"].([]any)
		b["citations"] = append(cites, d["citation"])
	case "input_json_delta":
		if s.inputs == nil {
			s.inputs = map[int]*bytes.Buffer{}
		}
		if s.inputs[index] == nil {
			s.inputs[index] = &bytes.Buffer{}
		}
		s.inputs[index].WriteString(str(d["partial_json"]))
	}
}

func (s *anthropicStreamBlocks) stop(index int) {
	if buf := s.inputs[index]; buf != nil && buf.Len() > 0 && index < len(s.blocks) && s.blocks[index] != nil {
		s.blocks[index]["input"] = json.RawMessage(buf.Bytes())
	}
}

// raw returns the blocks when any is of a kind only a verbatim replay
// keeps, nil otherwise.
func (s *anthropicStreamBlocks) raw() json.RawMessage {
	keep := false
	var out []map[string]any
	for _, b := range s.blocks {
		if b == nil {
			continue
		}
		if t := b["type"]; t != "text" && t != "tool_use" {
			keep = true
		}
		out = append(out, b)
	}
	if !keep {
		return nil
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return data
}

func (p *AnthropicProvider) StreamTurn(
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

		hints := agent.RequestHintsFrom(ctx)
		msgs := anthropicMessages(withoutImages(history, p.SupportsImages()))
		var betas []string

		// Tools, then system, then messages is the order the API caches
		// in. A breakpoint on the system block covers tools and system; one
		// on the last message covers the conversation, which the next
		// round-trip repeats. A one-off request with no tools (a compaction
		// summary) would pay the cache-write premium for nothing, so it
		// gets neither.
		var anthroTools []anthropicTool
		var system any = systemPrompt
		if len(tools) > 0 {
			if os.Getenv("ANTHROPIC_DISABLE_WEB_SEARCH") == "" {
				anthroTools = append(anthroTools, webSearchTool(p.Model))
			}
			if hints.AdvisorModel != "" && hints.AdvisorModel != p.Model {
				anthroTools = append(anthroTools, anthropicTool{Type: "advisor_20260301", Name: "advisor", Model: hints.AdvisorModel, Caching: ephemeral})
				betas = append(betas, "advisor-tool-2026-03-01")
			}
			for _, t := range tools {
				anthroTools = append(anthroTools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
			}
			if systemPrompt != "" {
				system = []*anthropicContentBlock{{Type: "text", Text: systemPrompt, CacheControl: ephemeral}}
			}
			markLastBlock(msgs)
		}

		// Adaptive-thinking models always think adaptively (a constant
		// setting: toggling thinking between requests invalidates the
		// cache), with effort as the depth control; "summarized" streams
		// readable reasoning instead of empty blocks. Older models only take
		// a fixed budget, which an effort level maps onto.
		maxTokens := 32000
		var thinking *anthropicThinking
		var outputConfig *anthropicOutputConfig
		effort, budget := p.getEffort(), int(p.thinkingBudget.Load())
		if adaptiveThinking(p.Model) {
			thinking = &anthropicThinking{Type: "adaptive", Display: "summarized"}
			if effort != "" {
				outputConfig = &anthropicOutputConfig{Effort: effort}
			}
		} else {
			maxTokens = 8192
			if b, ok := effortBudget[effort]; ok {
				budget = b
			}
			if budget > 0 {
				thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
				// max_tokens must exceed the thinking budget it's carved out of.
				if maxTokens <= budget {
					maxTokens = budget + 8192
				}
			}
		}

		reqBody := anthropicRequest{
			Model:        p.Model,
			System:       system,
			Messages:     msgs,
			Tools:        anthroTools,
			Stream:       true,
			MaxTokens:    maxTokens,
			Thinking:     thinking,
			OutputConfig: outputConfig,
		}

		jsonBytes, err := json.Marshal(reqBody)
		if err != nil {
			eChan <- err
			return
		}

		resp, err := p.post(ctx, jsonBytes, betas)
		if err != nil {
			eChan <- err
			return
		}
		body := newStallReader(resp.Body, streamIdleTimeout)
		defer body.Close()

		if resp.StatusCode != http.StatusOK {
			eChan <- httpError(resp, "anthropic")
			return
		}

		// Track streaming tool accumulation
		var currentToolCall *agent.ToolCall
		var currentArgsBuffer bytes.Buffer
		var blocks anthropicStreamBlocks

		var usage protocol.UsageData
		var stopReason string
		var sawEvent, sawStop bool
		// send hands a chunk on, or gives up once the turn is cancelled: the
		// consumer may have stopped reading.
		send := func(c agent.ModelStreamChunk) error {
			select {
			case cChan <- c:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Input usage arrives in message_start and, when server tools ran,
		// again (updated) in message_delta. input_tokens is only the
		// uncached part; InputTokens here is the whole prompt, as for every
		// other provider.
		type anthropicUsage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		}
		applyUsage := func(u anthropicUsage) {
			if in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; in > 0 {
				usage.InputTokens = in
				usage.CachedInputTokens = u.CacheReadInputTokens
				usage.CacheWriteInputTokens = u.CacheCreationInputTokens
			}
			if u.OutputTokens > 0 {
				usage.OutputTokens = u.OutputTokens
			}
		}

		if readErr := ReadSSEStream(body, func(ev SSEEvent) error {
			sawEvent = true
			switch ev.Event {
			case "message_stop":
				sawStop = true

			case "message_start":
				var msg struct {
					Message struct {
						Usage anthropicUsage `json:"usage"`
					} `json:"message"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &msg); err == nil {
					applyUsage(msg.Message.Usage)
				}

			case "message_delta":
				var d struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage anthropicUsage `json:"usage"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &d); err == nil {
					applyUsage(d.Usage)
					switch d.Delta.StopReason {
					case "max_tokens":
						stopReason = "length"
					case "refusal":
						stopReason = "refusal"
					case "pause_turn":
						// The server-side tool loop hit its limit and wants the
						// turn resent to continue it. Not done yet, so an answer
						// cut here must not pass for a finished one.
						return fmt.Errorf("anthropic paused the reply (pause_turn) before it finished")
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
					Index        int            `json:"index"`
					ContentBlock map[string]any `json:"content_block"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &block); err == nil {
					blocks.start(block.Index, block.ContentBlock)
					// Reset unconditionally: a server_tool_use block (e.g.
					// the web search query) also streams input_json_delta
					// into this same buffer, and leaving it unreset would
					// prefix the next real tool_use call's arguments with
					// stale bytes from the search query.
					currentArgsBuffer.Reset()
					if block.ContentBlock["type"] == "tool_use" {
						currentToolCall = &agent.ToolCall{
							ID:   fmt.Sprint(block.ContentBlock["id"]),
							Name: fmt.Sprint(block.ContentBlock["name"]),
						}
					} else {
						currentToolCall = nil
					}
				}

			case "content_block_delta":
				var delta struct {
					Index int            `json:"index"`
					Delta map[string]any `json:"delta"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &delta); err == nil {
					blocks.delta(delta.Index, delta.Delta)
					switch delta.Delta["type"] {
					case "text_delta":
						return send(agent.ModelStreamChunk{
							Kind:  agent.StreamChunkDelta,
							Delta: fmt.Sprint(delta.Delta["text"]),
						})
					case "thinking_delta":
						return send(agent.ModelStreamChunk{
							Kind:  agent.StreamChunkReasoning,
							Delta: fmt.Sprint(delta.Delta["thinking"]),
						})
					case "input_json_delta":
						currentArgsBuffer.WriteString(fmt.Sprint(delta.Delta["partial_json"]))
					}
				}

			case "content_block_stop":
				var stop struct {
					Index int `json:"index"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &stop)
				blocks.stop(stop.Index)
				if currentToolCall != nil {
					args := currentArgsBuffer.Bytes()
					if len(args) == 0 {
						args = []byte("{}")
					}
					currentToolCall.Args = json.RawMessage(append([]byte(nil), args...))
					call := currentToolCall
					currentToolCall = nil
					return send(agent.ModelStreamChunk{
						Kind:     agent.StreamChunkToolCall,
						ToolCall: call,
					})
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

		// Anthropic always ends a reply with message_stop. Without it the
		// connection closed partway through, and the answer is not whole.
		if sawEvent && !sawStop {
			eChan <- fmt.Errorf("anthropic stream ended before the reply finished: %w", io.ErrUnexpectedEOF)
			return
		}

		if raw := blocks.raw(); raw != nil {
			if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkRawContent, Raw: raw}); err != nil {
				eChan <- err
				return
			}
		}
		if stopReason != "" {
			if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkFinish, FinishReason: stopReason}); err != nil {
				eChan <- err
				return
			}
		}

		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		if err := send(agent.ModelStreamChunk{Kind: agent.StreamChunkUsage, Usage: &usage}); err != nil {
			eChan <- err
		}
	}()

	return cChan, eChan
}
