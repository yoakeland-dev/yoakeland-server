package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/tool"
)

// DefaultEchoProvider responds directly if no real LLM API token is present.
type DefaultEchoProvider struct{}

func (p *DefaultEchoProvider) StreamTurn(ctx context.Context, history []agent.Message) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 2)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		lastMsg := history[len(history)-1]
		if lastMsg.Role == "user" {
			cChan <- agent.ModelStreamChunk{
				Kind:  agent.StreamChunkDelta,
				Delta: fmt.Sprintf("Echo from yoakeland: %s", lastMsg.Content),
			}
		}
	}()

	return cChan, eChan
}

func main() {
	var projectDir string
	flag.StringVar(&projectDir, "project", ".", "Project root working directory")
	flag.Parse()

	norm, err := normalizer.New(projectDir)
	if err != nil {
		log.Fatalf("failed to initialize normalizer: %v", err)
	}

	sb := sandbox.NewLocalSandbox(norm.ProjectRoot, 30*time.Second)
	permEngine := permission.New(norm)
	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewBashTool(norm, sb),
	}

	ipc := protocol.NewIPCStream(os.Stdin, os.Stdout)
	ag := agent.NewAgent(ipc, &DefaultEchoProvider{}, tools, permEngine)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		env, err := ipc.ReadEnvelope()
		if err != nil {
			if errorsIsEOF(err) {
				return
			}
			_ = ipc.SendEvent(protocol.EventError, "", "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to read envelope: %v", err),
				Fatal:   false,
			})
			continue
		}

		switch env.Type {
		case protocol.EventUserPrompt:
			var data protocol.UserPromptData
			if err := json.Unmarshal(env.Data, &data); err != nil {
				_ = ipc.SendEvent(protocol.EventError, env.TurnID, "", protocol.ErrorData{
					Message: "malformed user_prompt payload",
				})
				continue
			}

			go func(tID, prompt string) {
				if err := ag.RunTurn(ctx, tID, prompt); err != nil {
					_ = ipc.SendEvent(protocol.EventError, tID, "", protocol.ErrorData{
						Message: err.Error(),
					})
				}
			}(env.TurnID, data.Prompt)

		case protocol.EventPermissionResponse:
			var data protocol.PermissionResponseData
			if err := json.Unmarshal(env.Data, &data); err == nil {
				ag.HandlePermissionResponse(env.ToolCallID, data.Allowed, data.Remember, "")
			}
		}
	}
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}
