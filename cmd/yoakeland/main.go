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
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

type DefaultEchoProvider struct{}

func (p *DefaultEchoProvider) StreamTurn(_ context.Context, history []agent.Message) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 2)
	eChan := make(chan error, 1)

	go func() {
		defer close(cChan)
		defer close(eChan)

		lastMsg := history[len(history)-1]
		if lastMsg.Role == "user" {
			cChan <- agent.ModelStreamChunk{
				Kind:  agent.StreamChunkDelta,
				Delta: fmt.Sprintf("Yoakeland received: %s", lastMsg.Content),
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

	// Phase 1 Subsystems
	permStore := permission.NewPersistentStore(norm.ProjectRoot)
	permEngine := permission.New(norm, permStore)

	snapStore, err := snapshot.NewDiskSnapshotStore(norm.ProjectRoot)
	if err != nil {
		log.Fatalf("failed to initialize snapshot store: %v", err)
	}

	sessStore, err := agent.NewDiskSessionStore(norm.ProjectRoot)
	if err != nil {
		log.Fatalf("failed to initialize session store: %v", err)
	}

	sb := sandbox.NewSandbox(norm.ProjectRoot, 30*time.Second)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
	}

	ctxMgr := agent.NewContextManager(32000, 2000)
	ipc := protocol.NewIPCStream(os.Stdin, os.Stdout)
	ag := agent.NewAgent(ipc, &DefaultEchoProvider{}, tools, permEngine, snapStore, sessStore, ctxMgr)

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
			if err == io.EOF {
				return
			}
			_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{
				Message: fmt.Sprintf("failed to read envelope: %v", err),
			})
			continue
		}

		switch env.Type {
		case protocol.EventUserPrompt:
			var data protocol.UserPromptData
			if err := json.Unmarshal(env.Data, &data); err != nil {
				_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
					Message: "malformed user_prompt payload",
				})
				continue
			}

			sessID := data.SessionID
			if sessID == "" {
				sessID = env.SessionID
			}
			if sessID == "" {
				sessID = "default"
			}

			go func(sID, tID, prompt string) {
				if err := ag.RunTurn(ctx, sID, tID, prompt); err != nil {
					_ = ipc.SendEvent(protocol.EventError, sID, tID, "", protocol.ErrorData{
						Message: err.Error(),
					})
				}
			}(sessID, env.TurnID, data.Prompt)

		case protocol.EventPermissionResponse:
			var data protocol.PermissionResponseData
			if err := json.Unmarshal(env.Data, &data); err == nil {
				ag.HandlePermissionResponse(env.ToolCallID, data.Allowed, data.Remember, "")
			}

		case protocol.EventRestoreSnapshot:
			var data protocol.RestoreSnapshotData
			if err := json.Unmarshal(env.Data, &data); err == nil {
				if rErr := snapStore.RestoreSnapshot(data.SnapshotID, norm.ProjectRoot); rErr != nil {
					_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{
						Message: fmt.Sprintf("failed to restore snapshot %s: %v", data.SnapshotID, rErr),
					})
				} else {
					_ = ipc.SendEvent(protocol.EventSnapshotRestored, env.SessionID, env.TurnID, "", protocol.SnapshotEventData{
						SnapshotID: data.SnapshotID,
					})
				}
			}
		}
	}
}
