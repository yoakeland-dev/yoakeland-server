package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
)

// ContextBreakdown asks the Claude Code process serving sessionKey how its
// context is split (agent.ContextInspector), with its get_context_usage
// control request. "full" has Claude Code count each category with the
// token-count API, so it is the exact answer but a slower one; if that fails
// or takes too long the cheap "summary" (its own local estimates) stands in.
// (nil, nil) when the provider isn't Claude or the chat has no live process.
func (p *CLISubprocessProvider) ContextBreakdown(ctx context.Context, sessionKey string) (*protocol.ContextBreakdownData, error) {
	if p.Kind != CLIKindClaude {
		return nil, nil
	}
	s, ok := liveSession(&agent.NativeTurn{SessionKey: sessionKey}, p.poolKind()).(*claudeSession)
	if !ok || s == nil {
		return nil, nil
	}
	ask := func(detail string, wait time.Duration) (*protocol.ContextBreakdownData, error) {
		rctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		r, err := s.request(rctx, "get_context_usage", map[string]any{"detail": detail})
		if err != nil {
			return nil, err
		}
		if e := controlError(r); e != "" {
			return nil, fmt.Errorf("claude: %s", e)
		}
		var resp struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(r.Response, &resp); err != nil {
			return nil, err
		}
		return parseClaudeContextUsage(resp.Response, detail == "full")
	}
	out, err := ask("full", 12*time.Second)
	if err != nil && ctx.Err() == nil {
		out, err = ask("summary", 5*time.Second)
	}
	return out, err
}

type claudeContextUsage struct {
	Categories []struct {
		Name       string `json:"name"`
		Tokens     int    `json:"tokens"`
		Kind       string `json:"kind"`
		IsDeferred bool   `json:"isDeferred"`
	} `json:"categories"`
	TotalTokens          int                     `json:"totalTokens"`
	MaxTokens            int                     `json:"maxTokens"`
	RawMaxTokens         int                     `json:"rawMaxTokens"`
	Model                string                  `json:"model"`
	AutoCompactThreshold int                     `json:"autoCompactThreshold"`
	IsAutoCompactEnabled bool                    `json:"isAutoCompactEnabled"`
	MessageBreakdown     *claudeMessageBreakdown `json:"messageBreakdown"`
	// APIUsage is the last API response's usage; null before the first one.
	APIUsage *struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"apiUsage"`
}

// claudeCategoryKey maps the CLI's row names onto our category keys; ""
// means it has no key of ours and is shown as "other" under its own name.
func claudeCategoryKey(name string) string {
	switch strings.ToLower(name) {
	case "system prompt":
		return protocol.CtxSystem
	case "system tools":
		return protocol.CtxTools
	case "mcp tools":
		return protocol.CtxMCP
	case "memory files":
		return protocol.CtxMemory
	case "skills":
		return protocol.CtxSkills
	case "custom agents":
		return protocol.CtxAgents
	case "messages":
		return protocol.CtxMessages
	}
	return ""
}

// parseClaudeContextUsage reads a get_context_usage response body. exact says
// the categories were asked for with detail "full", which counts them with the
// token-count API (when the CLI has an API response to count against).
// Deferred rows (tool schemas that aren't in the window) are left out. The
// Messages row is split by messageBreakdown when that agrees with it, else
// the split is scaled to fit and marked as an estimate.
func parseClaudeContextUsage(raw json.RawMessage, exact bool) (*protocol.ContextBreakdownData, error) {
	var u claudeContextUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("claude get_context_usage: %w", err)
	}
	if u.TotalTokens == 0 && len(u.Categories) == 0 {
		return nil, fmt.Errorf("claude get_context_usage: empty answer")
	}
	out := &protocol.ContextBreakdownData{
		Model:        u.Model,
		TotalTokens:  u.TotalTokens,
		WindowTokens: max(u.RawMaxTokens, u.MaxTokens),
		Source:       "estimated",
	}
	if u.RawMaxTokens > 0 {
		out.WindowTokens = u.RawMaxTokens
	}
	if u.IsAutoCompactEnabled {
		out.AutoCompactAt = u.AutoCompactThreshold
	}
	// Before the API has answered once (a fresh chat, or a login that has lapsed) the CLI has
	// nothing to count against and falls back to its own local estimates, whatever detail was
	// asked for: exact only with a real response behind it.
	exact = exact && u.APIUsage != nil
	if a := u.APIUsage; a != nil {
		out.Source = "provider"
		if in := a.InputTokens + a.CacheCreationInputTokens + a.CacheReadInputTokens; in > 0 {
			out.Cache = &protocol.ContextCache{Read: a.CacheReadInputTokens, Write: a.CacheCreationInputTokens, Input: in}
		}
	}
	var used, spare []protocol.ContextCategory
	for _, c := range u.Categories {
		if c.Tokens <= 0 || c.IsDeferred || c.Kind == "deferred" {
			continue
		}
		cat := protocol.ContextCategory{Tokens: c.Tokens, Exact: exact}
		switch c.Kind {
		case "free":
			cat.Key = protocol.CtxFree
		case "buffer":
			cat.Key = protocol.CtxBuffer
		default:
			cat.Key = claudeCategoryKey(c.Name)
		}
		cat.Label = agent.CategoryLabel(cat.Key)
		if cat.Key == "" {
			cat.Key, cat.Label = protocol.CtxOther, c.Name
		}
		if cat.Key == protocol.CtxFree || cat.Key == protocol.CtxBuffer {
			spare = append(spare, cat)
			continue
		}
		if cat.Key == protocol.CtxMessages && u.MessageBreakdown != nil {
			used = append(used, splitClaudeMessages(cat, u.MessageBreakdown.shares())...)
			continue
		}
		used = append(used, cat)
	}
	out.Categories = append(used, spare...)
	return out, nil
}

// claudeMessageBreakdown is how Claude Code splits its Messages row.
type claudeMessageBreakdown struct {
	ToolCallTokens          int `json:"toolCallTokens"`
	ToolResultTokens        int `json:"toolResultTokens"`
	AttachmentTokens        int `json:"attachmentTokens"`
	AssistantMessageTokens  int `json:"assistantMessageTokens"`
	UserMessageTokens       int `json:"userMessageTokens"`
	RedirectedContextTokens int `json:"redirectedContextTokens"`
	UnattributedTokens      int `json:"unattributedTokens"`
}

func (m *claudeMessageBreakdown) shares() map[string]int {
	return map[string]int{
		protocol.CtxUser:        m.UserMessageTokens,
		protocol.CtxAssistant:   m.AssistantMessageTokens,
		protocol.CtxToolCalls:   m.ToolCallTokens,
		protocol.CtxToolResults: m.ToolResultTokens,
		protocol.CtxAttachments: m.AttachmentTokens,
		protocol.CtxOther:       m.RedirectedContextTokens + m.UnattributedTokens,
	}
}

// splitClaudeMessages replaces the Messages row with its parts, scaled to add
// up to the row (the CLI's own two figures don't always agree, e.g. when an
// attachment it lists is counted under the system prompt). They stay exact
// only when they already agreed to within 3%; otherwise the split is ours.
func splitClaudeMessages(row protocol.ContextCategory, shares map[string]int) []protocol.ContextCategory {
	sum := 0
	for _, v := range shares {
		sum += v
	}
	parts := agent.ScaleShares(shares, row.Tokens)
	if len(parts) == 0 {
		return []protocol.ContextCategory{row}
	}
	agrees := sum > 0 && max(sum-row.Tokens, row.Tokens-sum)*100 <= row.Tokens*3
	for i := range parts {
		parts[i].Exact = row.Exact && agrees
	}
	return parts
}
