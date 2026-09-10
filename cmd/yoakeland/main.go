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
	"yoakeland/internal/provider"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

func main() {
	var projectDir string
	var providerName string
	var modelName string
	var apiKey string
	var baseURL string

	flag.StringVar(&projectDir, "project", ".", "Project root working directory")
	flag.StringVar(&providerName, "provider", "auto", "Provider: 'anthropic', 'openai', 'openrouter', or 'auto'")
	flag.StringVar(&modelName, "model", "", "Model name (defaults: claude-3-5-sonnet-20241022, gpt-4o, etc.)")
	flag.StringVar(&apiKey, "api-key", "", "API key (or use ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY)")
	flag.StringVar(&baseURL, "base-url", "", "Custom API base URL (e.g. http://localhost:1234/v1 for LM Studio)")
	flag.Parse()

	norm, err := normalizer.New(projectDir)
	if err != nil {
		log.Fatalf("failed to initialize normalizer: %v", err)
	}

	modelProv, err := provider.ResolveProvider(provider.Config{
		ProviderName: providerName,
		Model:        modelName,
		APIKey:       apiKey,
		BaseURL:      baseURL,
	})
	if err != nil {
		log.Fatalf("provider initialization failed: %v", err)
	}

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

	sb := sandbox.NewSandbox(norm.ProjectRoot, 60*time.Second)

	tools := []tool.Tool{
		tool.NewFileReadTool(norm),
		tool.NewFileWriteTool(norm),
		tool.NewFileEditTool(norm),
		tool.NewBashTool(norm, sb),
	}

	ctxMgr := agent.NewContextManager(64000, 4000)
	ipc := protocol.NewIPCStream(os.Stdin, os.Stdout)
	ag := agent.NewAgent(ipc, modelProv, tools, permEngine, snapStore, sessStore, ctxMgr)

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
				ag.HandlePermissionResponse(env.ToolCallID, data.Allowed, data.Remember)
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

		case protocol.EventCancelTurn:
			var data protocol.CancelTurnData
			_ = json.Unmarshal(env.Data, &data)
			tID := data.TurnID
			if tID == "" {
				tID = env.TurnID
			}
			ag.CancelTurn(tID)
		}
	}
}
