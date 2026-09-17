package agent

import (
	"fmt"
	"strings"
	"sync"
)

type ContextManager struct {
	MaxHistoryTokens    int
	MaxToolOutputTokens int

	// mu guards MaxHistoryTokens: set_model_options (SetMaxHistoryTokens)
	// can land from a different connection/goroutine than an in-flight
	// turn reading it (PrepareHistory, or the context-fullness report at
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

// TruncateToolOutput bounds giant file readings or bash stdout dumps.
func (cm *ContextManager) TruncateToolOutput(raw string) string {
	maxBytes := cm.MaxToolOutputTokens * 4
	if len(raw) <= maxBytes {
		return raw
	}

	half := maxBytes / 2
	head := raw[:half]
	tail := raw[len(raw)-half:]
	omitted := len(raw) - (half * 2)

	return fmt.Sprintf("%s\n\n[... %d bytes truncated by Yoakeland Context Manager ...]\n\n%s", head, omitted, tail)
}

// PrepareHistory applies sliding-window compaction when message history exceeds token limits.
func (cm *ContextManager) PrepareHistory(history []Message) []Message {
	totalTokens := 0
	for _, m := range history {
		totalTokens += cm.EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			totalTokens += cm.EstimateTokens(string(tc.Args))
		}
	}

	if totalTokens <= cm.GetMaxHistoryTokens() || len(history) <= 4 {
		return history
	}

	// Always retain the first user message (primary directive) and most recent 4 messages
	firstUserMsg := history[0]

	// The tail can't start on a "tool" message: both Anthropic and OpenAI
	// reject a request where a tool result's originating assistant
	// tool_calls message isn't present too (400 "tool_use ids were found
	// without tool_result blocks" / "'tool' message must be a response to a
	// preceding message with 'tool_calls'") — exactly the shape a long,
	// tool-heavy turn produces once it's long enough to trigger this naive
	// last-N slice. Widen the tail backward to the nearest assistant/user
	// message so every retained tool result keeps its pairing intact.
	tailStart := len(history) - 4
	for tailStart > 1 && history[tailStart].Role == "tool" {
		tailStart--
	}

	recentTail := history[tailStart:]
	discarded := history[1:tailStart]

	// Summarize discarded intermediate turns using their actual content
	// (CompactSummary) rather than dropping them with just a count — a bare
	// "N messages truncated" notice throws away whatever the model did in
	// the middle of a long session with no trace, which reads to the model
	// as amnesia rather than a bounded working set. Bound the summary's own
	// size with the same budget tool-output truncation uses, so a very long
	// discarded stretch can't blow the budget it exists to protect.
	summaryMsg := Message{
		Role: "system",
		Content: fmt.Sprintf("[%d intermediate conversation messages summarized to preserve context budget]\n%s",
			len(discarded), cm.TruncateToolOutput(cm.CompactSummary(discarded))),
	}

	compacted := make([]Message, 0, 6)
	compacted = append(compacted, firstUserMsg, summaryMsg)
	compacted = append(compacted, recentTail...)
	return compacted
}

func (cm *ContextManager) CompactSummary(history []Message) string {
	var sb strings.Builder
	for _, m := range history {
		if m.Role == "user" {
			sb.WriteString(fmt.Sprintf("User: %s\n", m.Content))
		} else if m.Role == "assistant" && m.Content != "" {
			sb.WriteString(fmt.Sprintf("Assistant: %s\n", m.Content))
		}
	}
	return sb.String()
}
