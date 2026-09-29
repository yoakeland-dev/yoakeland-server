package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/protocol"
)

type ContextManager struct {
	MaxHistoryTokens    int
	MaxToolOutputTokens int

	// mu guards MaxHistoryTokens: set_model_options (SetMaxHistoryTokens)
	// can land from a different connection/goroutine than an in-flight
	// turn reading it (maybeCompact, or the context-fullness report at
	// turn end), both against the field directly rather than an atomic —
	// this closes that race.
	mu sync.Mutex
}

func NewContextManager(maxHistoryTokens, maxToolOutputTokens int) *ContextManager {
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = 32000
	}
	if maxToolOutputTokens <= 0 {
		maxToolOutputTokens = 2000
	}
	return &ContextManager{
		MaxHistoryTokens:    maxHistoryTokens,
		MaxToolOutputTokens: maxToolOutputTokens,
	}
}

// SetMaxHistoryTokens updates the context-window budget (set_model_options'
// contextWindow). Safe against a concurrent read from an in-flight turn.
func (cm *ContextManager) SetMaxHistoryTokens(n int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.MaxHistoryTokens = n
}

// GetMaxHistoryTokens reads the context-window budget. Safe against a
// concurrent SetMaxHistoryTokens from another connection on the same
// session.
func (cm *ContextManager) GetMaxHistoryTokens() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.MaxHistoryTokens
}

// EstimateTokens calculates an approximate token count (~4 characters per token heuristic).
func (cm *ContextManager) EstimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// EstimateHistoryTokens sums the token estimate across an entire message
// history, including tool call arguments. Used to report context-window
// fullness to the frontend.
func (cm *ContextManager) EstimateHistoryTokens(history []Message) int {
	total := 0
	for _, m := range history {
		total += cm.EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			total += cm.EstimateTokens(string(tc.Args))
		}
	}
	return total
}

// TruncateToolOutput bounds giant file readings or bash stdout dumps,
// keeping the head and tail and naming the lines it cut, so the model can
// read exactly that range back if it needs it.
func (cm *ContextManager) TruncateToolOutput(raw string) string {
	maxBytes := cm.MaxToolOutputTokens * 4
	if len(raw) <= maxBytes {
		return raw
	}

	half := maxBytes / 2
	head := raw[:half]
	tail := raw[len(raw)-half:]
	omitted := raw[half : len(raw)-half]
	firstLine := strings.Count(head, "\n") + 1
	lastLine := firstLine + strings.Count(omitted, "\n")

	return fmt.Sprintf("%s\n\n[... lines %d-%d (%d bytes) of this output were cut to save context ...]\n\n%s",
		head, firstLine, lastLine, len(omitted), tail)
}

// Compaction kinds (SPEC.md §4g).
const (
	CompactionClear   = "clear"
	CompactionSummary = "summary"
)

// Compaction is what a "compaction" history message did. Compaction is
// recorded, never re-derived: contextView applies every compaction message
// in order, so the context the model sees only changes when a new one is
// appended, and consecutive requests share a byte-identical prefix — which
// prompt caching and Anthropic's thinking-block binding both depend on.
type Compaction struct {
	Kind string `json:"kind"`
	// ClearBefore (kind "clear"): tool results before this message, and
	// bulky tool-call arguments, are replaced by stubs.
	ClearBefore  string `json:"clearBefore,omitempty"`
	TokensBefore int    `json:"tokensBefore"`
	TokensAfter  int    `json:"tokensAfter"`
}

const summaryOpening = "[The conversation so far was summarized to save context. Summary:]\n"

// contextView renders history as the model sees it under the current
// prefixKey (see requestPrefixKey):
//   - the latest summary compaction replaces everything before it with one
//     user message holding the summary;
//   - clear compactions after it stub old tool results;
//   - RawContent (thinking blocks, server tool results) survives only on
//     the trailing run of assistant messages generated under prefixKey and
//     after the last compaction — Anthropic rejects a thinking block whose
//     conversation prefix changed, and only lets blocks go from the front.
func contextView(history []Message, prefixKey string) []Message {
	start, clearIdx, lastCompaction := 0, -1, -1
	summary := ""
	for i, m := range history {
		if m.Role != "compaction" || m.Compaction == nil {
			continue
		}
		lastCompaction = i
		switch m.Compaction.Kind {
		case CompactionSummary:
			start, summary, clearIdx = i+1, m.Content, -1
		case CompactionClear:
			if j, ok := indexOfMessage(history, m.Compaction.ClearBefore); ok && j > clearIdx {
				clearIdx = j
			}
		}
	}

	out := make([]Message, 0, len(history)-start+1)
	if summary != "" {
		out = append(out, Message{Role: "user", Content: summaryOpening + summary})
	}
	for i := start; i < len(history); i++ {
		m := history[i]
		if m.Role == "compaction" {
			continue
		}
		if i < clearIdx {
			m = stubbed(m)
		}
		if i < lastCompaction {
			m.RawContent = nil
		}
		out = append(out, m)
	}

	replay := true
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "assistant" {
			continue
		}
		if out[i].PrefixKey != prefixKey {
			replay = false
		}
		if !replay {
			out[i].RawContent = nil
		}
	}
	return out
}

func indexOfMessage(history []Message, id string) (int, bool) {
	for i, m := range history {
		if m.ID == id {
			return i, true
		}
	}
	return 0, false
}

// bulkyArgBytes is the tool-call argument size a clear compaction stubs
// (file_write/file_edit carry whole files there).
const bulkyArgBytes = 1000

// stubbed replaces a message's tool result, or its bulky tool-call
// arguments, with a short note of what was there.
func stubbed(m Message) Message {
	switch m.Role {
	case "tool":
		if !strings.HasPrefix(m.Content, "[cleared") {
			m.Content = fmt.Sprintf("[cleared to save context: this result was %d lines — run the tool again if you need it]",
				strings.Count(m.Content, "\n")+1)
		}
	case "assistant":
		if len(m.ToolCalls) == 0 {
			break
		}
		calls := make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			if len(tc.Args) > bulkyArgBytes {
				var args struct {
					Path string `json:"path,omitempty"`
				}
				_ = json.Unmarshal(tc.Args, &args)
				stub, _ := json.Marshal(struct {
					Path string `json:"path,omitempty"`
					Note string `json:"note"`
				}{args.Path, "arguments cleared to save context"})
				tc.Args = stub
			}
			calls[i] = tc
		}
		m.ToolCalls = calls
	}
	return m
}

// requestPrefixKey names what a thinking block is bound to besides the
// messages before it: the model, system prompt and tool set.
func requestPrefixKey(model, systemPrompt string, tools any) string {
	h := sha256.New()
	h.Write([]byte(model + "\x00" + systemPrompt + "\x00"))
	_ = json.NewEncoder(h).Encode(tools)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// contextTokens is how full the context is: the provider's own count for
// the last reply since the latest compaction plus an estimate of what was
// appended after it, or an estimate of the whole view when there's no such
// reply yet. overhead is the system prompt and tool definitions.
func (cm *ContextManager) contextTokens(history, view []Message, overhead int) int {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "compaction" {
			break
		}
		if m.Role == "assistant" && m.Usage != nil && m.Usage.InputTokens > 0 {
			return m.Usage.InputTokens + m.Usage.OutputTokens + cm.EstimateHistoryTokens(history[i+1:])
		}
	}
	return overhead + cm.EstimateHistoryTokens(view)
}

// planClear works out a clear compaction for a context of used tokens:
// stub tool results oldest first (never the latest round's, from
// keepFrom on) until the context is down to half the budget. It returns
// the message to clear before and the tokens that frees, or ok=false when
// there's nothing left to clear.
func (cm *ContextManager) planClear(history []Message, used, budget int) (clearBefore string, saved int, ok bool) {
	start, cleared := 0, -1
	for i, m := range history {
		if m.Role != "compaction" || m.Compaction == nil {
			continue
		}
		switch m.Compaction.Kind {
		case CompactionSummary:
			start, cleared = i+1, -1
		case CompactionClear:
			if j, found := indexOfMessage(history, m.Compaction.ClearBefore); found && j > cleared {
				cleared = j
			}
		}
	}
	keepFrom := len(history)
	for i := len(history) - 1; i >= start; i-- {
		if history[i].Role == "assistant" {
			keepFrom = i
			break
		}
	}
	idx := -1
	for i := max(start, cleared); i < keepFrom; i++ {
		m := history[i]
		saved += cm.EstimateHistoryTokens([]Message{m}) - cm.EstimateHistoryTokens([]Message{stubbed(m)})
		idx = i + 1
		if used-saved <= budget/2 {
			break
		}
	}
	if idx < 0 || saved <= 0 {
		return "", 0, false
	}
	return history[idx].ID, saved, true
}

// transcriptForSummary flattens a context view into plain text for the
// summarizer, with long tool results trimmed.
func transcriptForSummary(view []Message) string {
	var sb strings.Builder
	for _, m := range view {
		switch m.Role {
		case "user":
			fmt.Fprintf(&sb, "User: %s\n\n", m.Content)
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&sb, "Assistant: %s\n\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "Tool call %s %s\n", tc.Name, truncateRunes(string(tc.Args), 500))
			}
		case "tool":
			fmt.Fprintf(&sb, "Tool result: %s\n\n", truncateRunes(m.Content, 2000))
		}
	}
	return sb.String()
}

// fallbackSummary is the summary used when the summarizing model call
// fails: the conversation's own words, without tool output.
func fallbackSummary(view []Message) string {
	var sb strings.Builder
	for _, m := range view {
		if m.Role == "user" || (m.Role == "assistant" && m.Content != "") {
			fmt.Fprintf(&sb, "%s: %s\n", strings.ToUpper(m.Role[:1])+m.Role[1:], truncateRunes(m.Content, 2000))
		}
	}
	return truncateRunes(sb.String(), 16000)
}

const minCompactionBudget = 8000

const compactionSystemPrompt = `You summarize a coding agent's session so the work can continue from your summary alone — the agent will see nothing else from before it.
Keep: every request the user made and their constraints; decisions and why; files read or changed, with paths and what matters in them; commands run and their outcomes; errors hit and how they were resolved; and exactly what is still left to do, including the step in progress.
Be specific and dense: exact names, paths and values, no preamble, no advice.`

// maybeCompact records a compaction before a round-trip when the context
// has outgrown its budget (SPEC.md §4g): stubbing old tool results first
// and, only when that can't bring it under the budget, summarizing
// everything so far with the utility model. It returns the summarizer's
// usage (zero when it didn't run). overhead is the system prompt and tool
// definitions' size.
func (a *Agent) maybeCompact(ctx context.Context, turnID string, bound RoleBinding, overhead int) protocol.UsageData {
	cm := a.contextManager
	// Below this there's no room to work in after the system prompt and
	// tools, and compacting would just repeat every round.
	budget := max(cm.GetMaxHistoryTokens(), minCompactionBudget)
	a.mu.Lock()
	history := append([]Message(nil), a.history...)
	a.mu.Unlock()
	view := contextView(history, "")
	if len(view) < 3 {
		return protocol.UsageData{}
	}
	used := cm.contextTokens(history, view, overhead)
	if used <= budget {
		return protocol.UsageData{}
	}
	if clearBefore, saved, ok := cm.planClear(history, used, budget); ok && used-saved <= budget*3/4 {
		a.recordCompaction(turnID, Message{Role: "compaction", Compaction: &Compaction{
			Kind: CompactionClear, ClearBefore: clearBefore, TokensBefore: used, TokensAfter: used - saved,
		}})
		return protocol.UsageData{}
	}

	summarizer := a.roleOr(RoleUtility, bound)
	summary, usage := summarize(ctx, summarizer, view)
	a.recordCompaction(turnID, Message{
		Role:     "compaction",
		Content:  summary,
		Provider: summarizer.ProviderName,
		Model:    summarizer.Model,
		Usage:    &usage,
		Compaction: &Compaction{
			Kind: CompactionSummary, TokensBefore: used, TokensAfter: overhead + cm.EstimateTokens(summaryOpening+summary),
		},
	})
	return usage
}

// summarize asks b's model for a summary of view, falling back to the
// conversation's own words if that fails: compaction must never fail a turn.
func summarize(ctx context.Context, b RoleBinding, view []Message) (string, protocol.UsageData) {
	var usage protocol.UsageData
	if _, native := b.Provider.(NativeAgent); native || b.Provider == nil {
		return fallbackSummary(view), usage
	}
	chunks, errs := b.Provider.StreamTurn(ctx, []Message{{Role: "user", Content: transcriptForSummary(view)}}, nil, compactionSystemPrompt)
	var sb strings.Builder
	for c := range chunks {
		switch c.Kind {
		case StreamChunkDelta:
			sb.WriteString(c.Delta)
		case StreamChunkUsage:
			if c.Usage != nil {
				usage = *c.Usage
			}
		}
	}
	err := <-errs
	if !usage.CostKnown && b.Model != "" {
		usage.CostUSD = EstimateCostUSD(b.Model, usage)
	}
	if summary := strings.TrimSpace(sb.String()); err == nil && summary != "" {
		return summary, usage
	}
	log.Printf("compaction summary via %s failed (%v); using the conversation's own text", b.ProviderName, err)
	return fallbackSummary(view), usage
}

// recordCompaction appends a compaction message to history and tells the
// session's clients.
func (a *Agent) recordCompaction(turnID string, m Message) {
	a.mu.Lock()
	m.ID = a.nextMessageIDLocked()
	m.CreatedAt = time.Now()
	a.history = append(a.history, m)
	sessID := a.sessionID
	a.mu.Unlock()
	_ = a.getStream().SendEvent(protocol.EventContextCompacted, sessID, turnID, "", protocol.ContextCompactedData{
		MessageID:    m.ID,
		Kind:         m.Compaction.Kind,
		TokensBefore: m.Compaction.TokensBefore,
		TokensAfter:  m.Compaction.TokensAfter,
	})
}

// advisorModel is the advisor role's model for a main-model turn of this
// session, "" otherwise: plan-role turns already run on the strong model,
// and a subagent's executor may not pair with it.
func (a *Agent) advisorModel(bound RoleBinding) string {
	a.mu.Lock()
	main, sub := a.provider, a.systemPrompt != ""
	a.mu.Unlock()
	if sub || bound.Provider != main {
		return ""
	}
	if b := a.roleOr(RoleAdvisor, RoleBinding{}); b.Model != bound.Model {
		return b.Model
	}
	return ""
}
