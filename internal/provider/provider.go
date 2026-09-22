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
	StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition, systemPrompt string) (<-chan agent.ModelStreamChunk, <-chan error)
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
