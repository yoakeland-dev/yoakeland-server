package agent

import (
	"fmt"
	"strings"
)

type ContextManager struct {
	MaxHistoryTokens    int
	MaxToolOutputTokens int
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

// EstimateTokens calculates an approximate token count (~4 characters per token heuristic).
func (cm *ContextManager) EstimateTokens(text string) int {
	return (len(text) + 3) / 4
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

	if totalTokens <= cm.MaxHistoryTokens || len(history) <= 4 {
		return history
	}

	// Always retain the first user message (primary directive) and most recent 4 messages
	firstUserMsg := history[0]
	recentTail := history[len(history)-4:]

	// Summarize discarded intermediate turns
	discardedCount := len(history) - 5
	summaryMsg := Message{
		Role: "system",
		Content: fmt.Sprintf("[Notice: %d intermediate conversation messages truncated to preserve context budget]",
			discardedCount),
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
