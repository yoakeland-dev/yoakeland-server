package provider

import (
	"bufio"
	"context"
	"io"
	"strings"
	"yoakeland/internal/agent"
	"yoakeland/internal/tool"
)

type Provider interface {
	StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error)
}

type SSEEvent struct {
	Event string
	Data  string
}

// ReadSSESteam decodes standard text/event-stream chunks line by line
func ReadSSESteam(r io.Reader, handle func(ev SSEEvent) error) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 8*1024*1024)

	var currentEvent SSEEvent
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if currentEvent.Data != "" || currentEvent.Event != "" {
				if err := handle(currentEvent); err != nil {
					return err
				}
				currentEvent = SSEEvent{}
			}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataVal := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentEvent.Data == "" {
				currentEvent.Data = dataVal
			} else {
				currentEvent.Data += "\n" + dataVal
			}
		}
	}

	if currentEvent.Data != "" || currentEvent.Event != "" {
		return handle(currentEvent)
	}
	return scanner.Err()
}

// DefaultSystemPrompt establishes coding agent conventions
const DefaultSystemPrompt = `You are Yoakeland, an autonomous software engineering agent.
You have access to tools to inspect and modify code in the user's project:
- file_read: read file content
- file_write: create or overwrite files
- file_edit: perform precise search-and-replace edits
- bash: run commands inside the project sandbox

Rules:
1. Always read relevant files before proposing edits.
2. Ensure file edits are minimal and targeted.
3. Be concise. Deliver actionable responses without unnecessary preamble.`
