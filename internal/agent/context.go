package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

type ContextManager struct {
	// MaxHistoryTokens is the compaction budget: how big the context may
	// grow before maybeCompact shrinks it. Not the model's window (see
	// Window), though it is clamped to it.
	MaxHistoryTokens    int
	MaxToolOutputTokens int

	// mu guards MaxHistoryTokens and userWindow: set_model_options
	// (SetContextWindow) can land from a different connection/goroutine
	// than an in-flight turn reading them (maybeCompact, or the
	// context-fullness report at turn end), both against the fields
	// directly rather than an atomic — this closes that race.
	mu         sync.Mutex
	userWindow int // set_model_options' contextWindow; 0 = not set

	// compactAt and autoOff are this session's own say over when it
	// compacts (set_model_options autoCompactTokens / autoCompact), in
	// memory only: they last as long as the session's agent does. Guarded by
	// mu. 0 / false = follow the server's setting.
	compactAt int
	autoOff   bool
}

// Server-wide compaction settings (runtime settings autoCompactTokens and
// compactInstructions), set by SetCompactDefaults. 0 / "" = built-in budget
// and no extra instructions.
var (
	defaultCompactTokens       atomic.Int64
	defaultCompactInstructions atomic.Value // string
)

// SetCompactDefaults applies the server's compaction settings to every
// session, live ones included.
func SetCompactDefaults(tokens int, instructions string) {
	defaultCompactTokens.Store(int64(tokens))
	defaultCompactInstructions.Store(instructions)
}

// CompactTokensDefault is the server's autoCompactTokens setting (0 = not set),
// for the providers whose agent compacts itself and is told when to.
func CompactTokensDefault() int { return int(defaultCompactTokens.Load()) }

func compactInstructions() string {
	s, _ := defaultCompactInstructions.Load().(string)
	return s
}

// SetAutoCompact applies a session's overrides: at > 0 compacts at that size
// instead of the server's, at == 0 drops that override; auto false stops the
// session compacting itself. nil leaves either as it is.
func (cm *ContextManager) SetAutoCompact(at *int, auto *bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if at != nil {
		cm.compactAt = max(*at, 0)
	}
	if auto != nil {
		cm.autoOff = !*auto
	}
}

func (cm *ContextManager) autoCompactOff() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.autoOff
}

// contextWindowLookup finds a model's window from a catalog (OpenRouter's,
// then LiteLLM's); 0 when it has none. A hook, like livePricingSource,
// because the catalogs live in internal/provider, which imports this package.
var contextWindowLookup func(model string) int

// SetContextWindowLookup wires a catalog lookup into ContextManager.Window.
func SetContextWindowLookup(lookup func(model string) int) {
	contextWindowLookup = lookup
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

// SetContextWindow applies set_model_options' contextWindow: the user's word
// for the model's window, which also becomes the compaction budget (clamped
// back under the window by compactionBudget). Safe against a concurrent read
// from an in-flight turn.
func (cm *ContextManager) SetContextWindow(n int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.MaxHistoryTokens, cm.userWindow = n, n
}

// GetMaxHistoryTokens reads the compaction budget. Safe against a
// concurrent SetContextWindow from another connection on the same session.
func (cm *ContextManager) GetMaxHistoryTokens() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.MaxHistoryTokens
}

// Window is model's context window in tokens, 0 when nothing knows it. In
// order: the user's override, the window a provider reported on the latest
// reply from this model (a native agent knows its own; it survives a
// restart because the reply's usage is stored), then the catalogs
// (contextWindowLookup: OpenRouter's context_length, LiteLLM's
// max_input_tokens).
func (cm *ContextManager) Window(model string, history []Message) int {
	cm.mu.Lock()
	user := cm.userWindow
	cm.mu.Unlock()
	if user > 0 {
		return user
	}
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "assistant" && m.Usage != nil && m.Usage.WindowTokens > 0 && (m.Model == model || m.Model == "" || model == "") {
			return m.Usage.WindowTokens
		}
	}
	if contextWindowLookup != nil && model != "" {
		return contextWindowLookup(model)
	}
	return 0
}

// compactionBudget is the size maybeCompact lets the context reach: the
// configured budget, kept under the model's window (compacting at the very
// edge leaves no room for the reply) and above minCompactionBudget (below
// that there's no room to work in after the system prompt and tools, and
// compacting would just repeat every round). window 0 = unknown.
func (cm *ContextManager) compactionBudget(window int) int {
	cm.mu.Lock()
	budget, override := cm.MaxHistoryTokens, cm.compactAt
	cm.mu.Unlock()
	if d := int(defaultCompactTokens.Load()); d > 0 {
		budget = d // the server's setting beats a budget that only came from a window
	}
	if override > 0 {
		budget = override
	}
	if window > 0 {
		budget = min(budget, window*9/10)
	}
	budget = max(budget, minCompactionBudget)
	if window > 0 {
		budget = min(budget, window)
	}
	return budget
}

// Report is the context meter for history: what turn_completed and
// session_history carry. model is the one the next request goes to; overhead
// is the system prompt and tools, which only the estimate needs (a
// provider's own count already includes them).
func (cm *ContextManager) Report(history []Message, model string, overhead int) protocol.ContextData {
	if model == "" {
		for i := len(history) - 1; i >= 0 && model == ""; i-- {
			model = history[i].Model
		}
	}
	used, source := cm.contextTokens(history, contextView(history, ""), overhead)
	window := cm.Window(model, history)
	budget := cm.compactionBudget(window)
	off := cm.autoCompactOff()
	if off {
		budget = 0
	}
	return protocol.ContextData{
		EstimatedTokens: used,
		MaxTokens:       cmp.Or(window, budget),
		WindowTokens:    window,
		BudgetTokens:    budget,
		AutoCompactOff:  off,
		Source:          source,
	}
}

// EstimateTokens approximates a token count: ~4 bytes per token for ASCII,
// ~1.5 characters per token for anything else (CJK and other scripts cost
// far more than their UTF-8 length suggests to a byte-length heuristic).
func (cm *ContextManager) EstimateTokens(text string) int {
	ascii, wide := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			wide++
		}
	}
	return (ascii+3)/4 + (wide*2+2)/3
}

// imageTokens is the flat allowance for an image attachment, whose size in
// the context depends on the provider and can't be known from the path.
const imageTokens = 1500

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// imageSummary describes attached pictures in words — a type and a size are
// all a text-only reader (the summarizer) can be told about one.
func imageSummary(images []protocol.ImageData) string {
	parts := make([]string, 0, len(images))
	for _, img := range images {
		dim := "size not reported"
		if img.Width > 0 && img.Height > 0 {
			dim = fmt.Sprintf("%dx%d", img.Width, img.Height)
		}
		parts = append(parts, img.MimeType+" "+dim)
	}
	return strings.Join(parts, ", ")
}

// EstimateHistoryTokens sums the token estimate across an entire message
// history: content, reasoning (or the raw provider blocks that carry it),
// tool call arguments, and attachments. Used to report context-window
// fullness to the frontend and to decide when to compact.
func (cm *ContextManager) EstimateHistoryTokens(history []Message) int {
	total := 0
	for _, m := range history {
		total += cm.EstimateTokens(m.Content)
		if len(m.RawContent) > 0 {
			// Replayed verbatim (contextView drops it where it isn't), and
			// it already holds the thinking text Reasoning shows.
			total += cm.EstimateTokens(string(m.RawContent))
		} else {
			total += cm.EstimateTokens(m.Reasoning)
		}
		for _, tc := range m.ToolCalls {
			total += cm.EstimateTokens(string(tc.Args))
		}
		for _, path := range m.Attachments {
			total += cm.EstimateTokens(path)
			if imageExts[strings.ToLower(filepath.Ext(path))] {
				total += imageTokens
			}
		}
		// An attached picture (a browser screenshot) is priced like an image
		// attachment: a flat allowance, since the provider decides the real
		// cost from the pixels.
		total += imageTokens * len(m.Images)
	}
	return total
}

// estimateOverhead sizes what every request carries besides the history:
// the system prompt and the tool definitions as they go on the wire.
func (cm *ContextManager) estimateOverhead(systemPrompt string, toolDefs []tool.ToolDefinition) int {
	defs, _ := json.Marshal(toolDefs)
	return cm.EstimateTokens(systemPrompt) + cm.EstimateTokens(string(defs))
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

	out = pairToolCalls(out)

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

// pairToolCalls gives every API tool call in view a tool result. A turn that
// was cancelled, failed or panicked mid-round leaves calls with no result,
// and providers reject the whole request for an unanswered tool_use. The
// synthetic result goes after the call's own results, in the view only;
// history keeps what really happened. Native calls are flattened to text for
// API providers, so they are left alone.
func pairToolCalls(view []Message) []Message {
	answered := map[string]bool{}
	for _, m := range view {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	out := make([]Message, 0, len(view))
	for i := 0; i < len(view); i++ {
		m := view[i]
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		j := i + 1
		for ; j < len(view) && view[j].Role == "tool"; j++ {
			out = append(out, view[j])
		}
		for _, tc := range m.ToolCalls {
			if !tc.Native && !answered[tc.ID] {
				out = append(out, Message{Role: "tool", ToolCallID: tc.ID, Content: "no result: the turn ended before this tool ran"})
				answered[tc.ID] = true
			}
		}
		i = j - 1
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
		// A picture (browser screenshot) is the most expensive thing a tool
		// result can carry, so it goes with the text: keeping it would leave
		// the thing compaction was trying to shrink.
		m.Images = nil
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

// requestOverhead is estimateOverhead for a request the session would make
// now; 0 for a native agent, which brings its own system prompt and tools.
func (a *Agent) requestOverhead() int {
	if _, native := a.getProvider().(NativeAgent); native {
		return 0
	}
	return a.contextManager.estimateOverhead(a.activeSystemPrompt()+a.requestSuffix(a.getProvider()), a.activeToolDefs())
}

// contextTokens is how full the context is, and where that figure came from
// (protocol.ContextData.Source): the provider's own count for the last reply
// since the latest compaction plus an estimate of what was appended after
// it, or an estimate of the whole view when there's no such reply yet.
// overhead is the system prompt and tool definitions.
//
// The count is the last request's input plus its reply. A reply's usage is
// only that when it is one request's (an API provider's, or a native
// agent's ContextTokens snapshot); the sum over a native turn's requests
// (UsageIsAggregate) re-counts the resent history every step and would read
// as far more than the context holds, so it is skipped.
// For Anthropic with extended thinking the reply's output tokens include
// thinking that may not be resent next request, so this can run slightly
// high right after such a reply; the next request's count corrects it.
func (cm *ContextManager) contextTokens(history, view []Message, overhead int) (int, string) {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "compaction" {
			break
		}
		if m.Role != "assistant" || m.Usage == nil {
			continue
		}
		reported := m.Usage.ContextTokens
		if reported == 0 && !m.UsageIsAggregate {
			reported = m.Usage.InputTokens + m.Usage.OutputTokens
		}
		if reported > 0 {
			return reported + cm.EstimateHistoryTokens(history[i+1:]), "provider"
		}
	}
	if len(view) == 0 {
		return 0, "unknown"
	}
	return overhead + cm.EstimateHistoryTokens(view), "estimated"
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
			if len(m.Images) > 0 {
				// The summarizer sees text only, so say the picture was there
				// and what it was of, or the summary loses it entirely.
				fmt.Fprintf(&sb, "[%d image(s) attached to that result: %s]\n\n", len(m.Images), imageSummary(m.Images))
			}
		}
	}
	return sb.String()
}

// fallbackSummary is the summary for a binding with no API model to ask (a
// native agent keeps its own context): the conversation's own words, without
// tool output.
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

const compactionBasePrompt = `You summarize a coding agent's session so the work can continue from your summary alone — the agent will see nothing else from before it.
Keep: every request the user made and their constraints; decisions and why; files read or changed, with paths and what matters in them; commands run and their outcomes; errors hit and how they were resolved; and exactly what is still left to do, including the step in progress.
Be specific and dense: exact names, paths and values, no preamble, no advice.`

// withInstructions adds a user's own guidance to the summarizer's prompt. It
// follows the base prompt and says it can't override it, so a careless
// "keep it short" narrows the emphasis instead of dropping what the agent
// needs to continue.
func withInstructions(extra string) string {
	if extra == "" {
		return compactionBasePrompt
	}
	return compactionBasePrompt + "\n\nThe user added this guidance for the summary. Follow it for what to emphasize, but it does not replace anything above: still keep every request, decision, changed file and the remaining work.\n<guidance>\n" + extra + "\n</guidance>"
}

// minSummaryTokens is how short a summary of used tokens can get before it
// can't be a summary of that much work: 1% of it, between 200 and 1500.
func minSummaryTokens(used int) int { return min(max(used/100, 200), 1500) }

// maybeCompact records a compaction before a round-trip when the context
// has outgrown its budget (SPEC.md §4g): stubbing old tool results first
// and, only when that can't bring it under the budget, summarizing
// everything so far with the utility model. It returns the summarizer's
// usage (zero when it didn't run). overhead is the system prompt and tool
// definitions' size.
func (a *Agent) maybeCompact(ctx context.Context, turnID string, bound RoleBinding, overhead int) protocol.UsageData {
	cm := a.contextManager
	a.mu.Lock()
	history := append([]Message(nil), a.history...)
	a.mu.Unlock()
	if cm.autoCompactOff() {
		return protocol.UsageData{}
	}
	budget := cm.compactionBudget(cm.Window(bound.Model, history))
	view := contextView(history, "")
	if len(view) < 3 {
		return protocol.UsageData{}
	}
	used, _ := cm.contextTokens(history, view, overhead)
	if used <= budget {
		return protocol.UsageData{}
	}
	if clearBefore, saved, ok := cm.planClear(history, used, budget); ok && used-saved <= budget*3/4 {
		a.recordCompaction(turnID, Message{Role: "compaction", Compaction: &Compaction{
			Kind: CompactionClear, ClearBefore: clearBefore, TokensBefore: used, TokensAfter: used - saved,
		}}, false)
		return protocol.UsageData{}
	}

	usage, err := a.compactBySummary(ctx, turnID, bound, view, used, overhead, "", false)
	if err != nil {
		log.Printf("session %s: compaction failed (%v); keeping the full context and trying again next round", a.CurrentSessionID(), err)
	}
	return usage
}

// compactBySummary records a summary compaction of view, which holds used
// tokens, and returns the summarizer's usage. It records nothing when the
// summary fails. extra is the caller's one-off guidance, added after the
// server's compactInstructions.
func (a *Agent) compactBySummary(ctx context.Context, turnID string, bound RoleBinding, view []Message, used, overhead int, extra string, manual bool) (protocol.UsageData, error) {
	summarizer := a.roleOr(RoleUtility, bound)
	instructions := strings.TrimSpace(compactInstructions() + "\n" + extra)
	summary, usage, err := summarize(ctx, summarizer, view, instructions, minSummaryTokens(used), a.contextManager)
	if err != nil {
		return usage, err
	}
	a.recordCompaction(turnID, Message{
		Role:     "compaction",
		Content:  summary,
		Provider: summarizer.ProviderName,
		Model:    summarizer.Model,
		Usage:    &usage,
		Compaction: &Compaction{
			Kind: CompactionSummary, TokensBefore: used, TokensAfter: overhead + a.contextManager.EstimateTokens(summaryOpening+summary),
		},
	}, manual)
	return usage, nil
}

// Compact summarizes the session's conversation now (compact_context).
// Refused while a turn runs, since a turn owns the history, and for a native
// agent, which compacts on its own terms. Persists the result and counts the
// summarizer's cost towards the session's spend.
func (a *Agent) Compact(ctx context.Context, sessionID, turnID, extra string) error {
	select {
	case a.turnSlot <- struct{}{}:
		defer a.unlockTurn()
	default:
		return errors.New("a turn is running; compact once it finishes")
	}
	a.mu.Lock()
	if a.sessionID == "" || a.sessionID != sessionID {
		a.sessionID = sessionID
	}
	history := append([]Message(nil), a.history...)
	a.mu.Unlock()

	bound := a.turnBinding()
	if bound.Provider == nil {
		return errors.New("no provider configured for this session")
	}
	if _, native := bound.Provider.(NativeAgent); native {
		return errors.New("this agent compacts its own context")
	}
	view := contextView(history, "")
	if len(view) < 2 {
		return errors.New("nothing to compact yet")
	}
	overhead := a.requestOverhead()
	used, _ := a.contextManager.contextTokens(history, view, overhead)
	usage, err := a.compactBySummary(ctx, turnID, bound, view, used, overhead, extra, true)
	if a.usageStore != nil {
		if usage.CostUnknown {
			a.usageStore.MarkCostUnknown(sessionID)
		}
		a.usageStore.Record(sessionID, usage.InputTokens, usage.OutputTokens, usage.CostUSD)
	}
	if err != nil {
		return fmt.Errorf("compaction failed, the conversation is unchanged: %w", err)
	}
	return a.persistCurrentState()
}

// summarize asks b's model for a summary of view. A failure is returned, not
// replaced with fallbackSummary: that would stand in for the whole
// conversation from then on, so the caller keeps the full history instead.
// instructions are the user's own guidance; if they bring back a summary
// shorter than minTokens it is thrown away and the base prompt runs alone, so
// a careless instruction can't leave the agent with nothing to go on.
func summarize(ctx context.Context, b RoleBinding, view []Message, instructions string, minTokens int, cm *ContextManager) (string, protocol.UsageData, error) {
	var usage protocol.UsageData
	if _, native := b.Provider.(NativeAgent); native || b.Provider == nil {
		return fallbackSummary(view), usage, nil
	}
	summary, u, err := requestSummary(ctx, b, view, withInstructions(instructions))
	usage = u
	if err == nil && instructions != "" && cm.EstimateTokens(summary) < minTokens {
		log.Printf("compaction summary with custom instructions was only %d tokens (< %d); redoing it without them", cm.EstimateTokens(summary), minTokens)
		summary, u, err = requestSummary(ctx, b, view, withInstructions(""))
		addUsage(&usage, u)
		usage.CostUSD += u.CostUSD
	}
	if err != nil {
		return "", usage, fmt.Errorf("summary via %s: %w", b.ProviderName, err)
	}
	if summary == "" {
		return "", usage, fmt.Errorf("summary via %s: empty", b.ProviderName)
	}
	return summary, usage, nil
}

func requestSummary(ctx context.Context, b RoleBinding, view []Message, system string) (string, protocol.UsageData, error) {
	var usage protocol.UsageData
	chunks, errs := b.Provider.StreamTurn(ctx, []Message{{Role: "user", Content: transcriptForSummary(view)}}, nil, system)
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
	priceUsage(b.Model, &usage)
	return strings.TrimSpace(sb.String()), usage, err
}

// recordCompaction appends a compaction message to history and tells the
// session's clients.
func (a *Agent) recordCompaction(turnID string, m Message, manual bool) {
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
		Summary:      m.Content,
		Manual:       manual,
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
