package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// ContextInspector is a NativeAgent that can say how its own context is
// split, because it counts it itself (Claude Code's get_context_usage). It
// answers for the CLI process serving sessionKey (NativeSessionKey), or
// (nil, nil) when there is no such process to ask: an idle chat's process is
// closed after a while, and nothing here starts one just to be asked.
type ContextInspector interface {
	ContextBreakdown(ctx context.Context, sessionKey string) (*protocol.ContextBreakdownData, error)
}

var categoryLabels = map[string]string{
	protocol.CtxSystem:      "System prompt",
	protocol.CtxTools:       "Tool definitions",
	protocol.CtxMemory:      "Memory files",
	protocol.CtxSkills:      "Skills",
	protocol.CtxAgents:      "Custom agents",
	protocol.CtxMCP:         "MCP tools",
	protocol.CtxUser:        "Your messages",
	protocol.CtxAssistant:   "Replies",
	protocol.CtxToolCalls:   "Tool calls",
	protocol.CtxToolResults: "Tool results",
	protocol.CtxReasoning:   "Reasoning",
	protocol.CtxAttachments: "Attachments",
	protocol.CtxSummary:     "Compaction summary",
	protocol.CtxMessages:    "Messages",
	protocol.CtxOther:       "Other",
	protocol.CtxFree:        "Free space",
	protocol.CtxBuffer:      "Reserved for compaction",
}

// CategoryLabel is the label a category key prints as.
func CategoryLabel(key string) string { return categoryLabels[key] }

// categoryOrder is the order used categories are listed in.
var categoryOrder = []string{
	protocol.CtxSystem, protocol.CtxTools, protocol.CtxMCP, protocol.CtxMemory, protocol.CtxSkills, protocol.CtxAgents,
	protocol.CtxSummary, protocol.CtxUser, protocol.CtxAssistant, protocol.CtxReasoning,
	protocol.CtxToolCalls, protocol.CtxToolResults, protocol.CtxAttachments, protocol.CtxMessages, protocol.CtxOther,
}

// estimateShares sizes each category of a request by character count:
// the system prompt and tool definitions, then what contextView leaves of the
// history, by kind. The numbers are proportions, not token counts (see
// ScaleShares).
func (cm *ContextManager) estimateShares(view []Message, systemPrompt string, tools []tool.ToolDefinition) map[string]int {
	var builtin, mcp []tool.ToolDefinition
	for _, d := range tools {
		if isMCPTool(d.Name) {
			mcp = append(mcp, d)
		} else {
			builtin = append(builtin, d)
		}
	}
	defs, _ := json.Marshal(builtin)
	mcpDefs, _ := json.Marshal(mcp)
	system, skillsPart := splitSkillsSection(systemPrompt)
	sh := map[string]int{
		protocol.CtxSystem: cm.EstimateTokens(system),
		protocol.CtxTools:  cm.EstimateTokens(string(defs)),
	}
	if len(mcp) > 0 {
		sh[protocol.CtxMCP] = cm.EstimateTokens(string(mcpDefs))
	}
	if skillsPart != "" {
		sh[protocol.CtxSkills] = cm.EstimateTokens(skillsPart)
	}
	for _, m := range view {
		content := cm.EstimateTokens(m.Content)
		calls := 0
		for _, tc := range m.ToolCalls {
			calls += cm.EstimateTokens(string(tc.Args))
		}
		switch m.Role {
		case "tool":
			sh[protocol.CtxToolResults] += content
		case "assistant":
			sh[protocol.CtxAssistant] += content
			// The raw blocks carry the thinking text and duplicate the reply
			// and calls; what they hold beyond those is the reasoning.
			if len(m.RawContent) > 0 {
				sh[protocol.CtxReasoning] += max(0, cm.EstimateTokens(string(m.RawContent))-content-calls)
			} else {
				sh[protocol.CtxReasoning] += cm.EstimateTokens(m.Reasoning)
			}
		case "user":
			if strings.HasPrefix(m.Content, summaryOpening) {
				sh[protocol.CtxSummary] += content
			} else {
				sh[protocol.CtxUser] += content
			}
		default:
			sh[protocol.CtxUser] += content
		}
		sh[protocol.CtxToolCalls] += calls
		for _, path := range m.Attachments {
			sh[protocol.CtxAttachments] += cm.EstimateTokens(path)
			if imageExts[strings.ToLower(filepath.Ext(path))] {
				sh[protocol.CtxAttachments] += imageTokens
			}
		}
	}
	return sh
}

// ScaleShares turns proportions into token counts adding up to exactly total
// (largest remainder), in categoryOrder, leaving out empty ones. total is
// what the provider says the request held; the proportions are only our
// estimate of how that splits.
func ScaleShares(shares map[string]int, total int) []protocol.ContextCategory {
	sum := 0
	for _, v := range shares {
		sum += v
	}
	if sum <= 0 || total <= 0 {
		return nil
	}
	type part struct {
		key       string
		tokens    int
		remainder int64
	}
	var parts []part
	given := 0
	for _, key := range categoryOrder {
		v := shares[key]
		if v <= 0 {
			continue
		}
		n := int64(v) * int64(total)
		parts = append(parts, part{key: key, tokens: int(n / int64(sum)), remainder: n % int64(sum)})
		given += parts[len(parts)-1].tokens
	}
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return parts[order[a]].remainder > parts[order[b]].remainder })
	for i := 0; given < total && len(order) > 0; i++ {
		parts[order[i%len(order)]].tokens++
		given++
	}
	out := make([]protocol.ContextCategory, 0, len(parts))
	for _, p := range parts {
		if p.tokens > 0 {
			out = append(out, protocol.ContextCategory{Key: p.key, Label: categoryLabels[p.key], Tokens: p.tokens})
		}
	}
	return out
}

// spareCategories are the two slices after the used ones: free is the room
// left before the context is compacted (budget, or the window itself when
// the agent doesn't compact on our terms), buffer the rest of the window
// beyond that point.
func spareCategories(used, window, budget int) []protocol.ContextCategory {
	limit := budget
	if limit <= 0 || (window > 0 && limit > window) {
		limit = window
	}
	var out []protocol.ContextCategory
	if free := limit - used; free > 0 {
		out = append(out, protocol.ContextCategory{Key: protocol.CtxFree, Label: categoryLabels[protocol.CtxFree], Tokens: free})
	}
	if window > 0 {
		if buf := window - max(used, limit); buf > 0 {
			out = append(out, protocol.ContextCategory{Key: protocol.CtxBuffer, Label: categoryLabels[protocol.CtxBuffer], Tokens: buf})
		}
	}
	return out
}

// lastRequestCache is what the latest reply's request read from and wrote to
// the prompt cache, since the last compaction; nil when the provider gave no
// per-request figure (a native agent's usage is summed over a whole turn).
func lastRequestCache(history []Message) *protocol.ContextCache {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "compaction" {
			return nil
		}
		if m.Role != "assistant" || m.Usage == nil || m.UsageIsAggregate || m.Usage.InputTokens == 0 {
			continue
		}
		return &protocol.ContextCache{Read: m.Usage.CachedInputTokens, Write: m.Usage.CacheWriteInputTokens, Input: m.Usage.InputTokens}
	}
	return nil
}

// Breakdown splits history's context for the model's next request into
// categories. The total is Report's: the provider's own count when it gave
// one, which the estimated shares are scaled to, else the estimate itself.
// native marks an agent with a system prompt and tools of its own, which
// can't be sized here: only the total is known then, unless its own
// breakdown replaces this one (see ContextInspector).
func (cm *ContextManager) Breakdown(history []Message, model, systemPrompt string, tools []tool.ToolDefinition, native bool) protocol.ContextBreakdownData {
	overhead := 0
	if !native {
		overhead = cm.estimateOverhead(systemPrompt, tools)
	}
	rep := cm.Report(history, model, overhead)
	out := protocol.ContextBreakdownData{
		Model:        model,
		TotalTokens:  rep.EstimatedTokens,
		WindowTokens: rep.WindowTokens,
		Source:       rep.Source,
		Cache:        lastRequestCache(history),
	}
	budget := 0
	if !native {
		budget = rep.BudgetTokens
		out.AutoCompactAt = rep.BudgetTokens
	}

	switch {
	case native:
		if rep.EstimatedTokens > 0 {
			out.Categories = []protocol.ContextCategory{{
				Key: protocol.CtxOther, Label: "In use (not itemized)", Tokens: rep.EstimatedTokens, Exact: rep.Source == "provider",
			}}
		}
	case rep.Source == "unknown":
		// Nothing said yet, but the fixed part of every request is known.
		out.Categories = ScaleShares(cm.estimateShares(nil, systemPrompt, tools), overhead)
		out.TotalTokens, out.Source = overhead, "estimated"
	default:
		view := contextView(history, requestPrefixKey(model, systemPrompt, tools))
		out.Categories = ScaleShares(cm.estimateShares(view, systemPrompt, tools), rep.EstimatedTokens)
	}
	out.Categories = append(out.Categories, spareCategories(out.TotalTokens, rep.WindowTokens, budget)...)
	return out
}

// ContextBreakdown answers get_context_breakdown for this session: the CLI's
// own numbers where the agent counts them, else the split Breakdown works
// out. It may wait on a native process, so call it off the read loop.
func (a *Agent) ContextBreakdown(ctx context.Context) protocol.ContextBreakdownData {
	a.mu.Lock()
	history := append([]Message(nil), a.history...)
	sessionID, root := a.sessionID, a.projectRoot
	a.mu.Unlock()

	provider := a.getProvider()
	_, native := provider.(NativeAgent)
	model := a.getModelName()
	out := a.contextManager.Breakdown(history, model, a.activeSystemPrompt()+a.requestSuffix(provider), a.activeToolDefs(), native)
	if insp, ok := provider.(ContextInspector); ok {
		cctx, cancel := context.WithTimeout(ctx, 18*time.Second)
		defer cancel()
		if own, err := insp.ContextBreakdown(cctx, NativeSessionKey(sessionID, root)); err == nil && own != nil {
			// Its numbers replace ours; only what it doesn't know stays.
			if own.Model == "" {
				own.Model = model
			}
			if own.Cache == nil {
				own.Cache = out.Cache
			}
			out = *own
		}
	}
	out.SessionID = sessionID
	if a.usageStore != nil {
		sess, proj := a.usageStore.SessionTotal(sessionID), a.usageStore.ProjectTotal()
		out.Spend = &protocol.SpendData{
			SessionTotalUSD: sess.CostUSD, ProjectTotalUSD: proj.CostUSD,
			SessionCostUnknown: sess.CostUnknown, ProjectCostUnknown: proj.CostUnknown,
		}
	}
	return out
}

// ContextBreakdown is Agent.ContextBreakdown for a session that has no live
// Agent (a chat nobody has opened since the server started): what history
// alone can say, with the registry's shared settings and no native process to ask.
func (r *Registry) ContextBreakdown(ctx context.Context, sessionID string, history []Message) protocol.ContextBreakdownData {
	if ag := r.Lookup(sessionID); ag != nil {
		return ag.ContextBreakdown(ctx)
	}
	out := r.ctxMgr.Breakdown(history, "", "", nil, true)
	out.SessionID = sessionID
	if r.usageStore != nil {
		sess, proj := r.usageStore.SessionTotal(sessionID), r.usageStore.ProjectTotal()
		out.Spend = &protocol.SpendData{
			SessionTotalUSD: sess.CostUSD, ProjectTotalUSD: proj.CostUSD,
			SessionCostUnknown: sess.CostUnknown, ProjectCostUnknown: proj.CostUnknown,
		}
	}
	return out
}
