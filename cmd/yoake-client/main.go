package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"yoakeland/internal/protocol"
)

func main() {
	var backendBin string
	var sessionID string
	flag.StringVar(&backendBin, "bin", "./yoakeland", "Path to yoakeland backend binary")
	flag.StringVar(&sessionID, "session", "session-main", "Session ID to join or resume")
	flag.Parse()

	cmd := exec.Command(backendBin)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting stdin pipe: %v\n", err)
		os.Exit(1)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting stdout pipe: %v\n", err)
		os.Exit(1)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start backend: %v\n", err)
		os.Exit(1)
	}

	clientStream := protocol.NewIPCStream(stdoutPipe, stdinPipe)
	var turnCounter uint64

	go func() {
		for {
			env, err := clientStream.ReadEnvelope()
			if err != nil {
				if err == io.EOF {
					fmt.Println("\n[client] Backend closed.")
					os.Exit(0)
				}
				return
			}

			switch env.Type {
			case protocol.EventModelDelta:
				var d protocol.ModelDeltaData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Print(d.Content)

			case protocol.EventToolCallRequested:
				var d protocol.ToolCallData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[TOOL CALL] %s -> %s\n", d.Name, string(d.Args))

			case protocol.EventPermissionRequest:
				var d protocol.PermissionRequestData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[PERMISSION REQUIRED] %s (%s)\nAllow (y/n, append 'r' to remember, e.g. 'yr'): ", d.ToolName, d.Description)

				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				trimmed := strings.ToLower(strings.TrimSpace(line))
				allow := strings.HasPrefix(trimmed, "y")
				remember := strings.Contains(trimmed, "r")

				_ = clientStream.SendEvent(
					protocol.EventPermissionResponse,
					env.SessionID,
					env.TurnID,
					env.ToolCallID,
					protocol.PermissionResponseData{Allowed: allow, Remember: remember},
				)

			case protocol.EventToolResult:
				var d protocol.ToolResultData
				_ = json.Unmarshal(env.Data, &d)
				if d.Error != "" {
					fmt.Printf("[TOOL ERR] %s: %s\n", d.ToolName, d.Error)
				} else {
					fmt.Printf("[TOOL OUT] %s:\n%s\n", d.ToolName, d.Output)
				}

			case protocol.EventTurnCompleted:
				fmt.Printf("\n--- [Turn completed (Session: %s)] ---\n> ", env.SessionID)

			case protocol.EventSnapshotRestored:
				var d protocol.SnapshotEventData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[SNAPSHOT RESTORED] Snapshot ID: %s\n> ", d.SnapshotID)

			case protocol.EventError:
				var d protocol.ErrorData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[BACKEND ERROR] %s\n> ", d.Message)
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("yoakeland client connected [Session: %s]\nCommands: 'restore <snap_id>' or enter a prompt:\n> ", sessionID)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		if line == "exit" || line == "quit" {
			_ = cmd.Process.Kill()
			break
		}

		if strings.HasPrefix(line, "restore ") {
			snapID := strings.TrimSpace(strings.TrimPrefix(line, "restore "))
			_ = clientStream.SendEvent(protocol.EventRestoreSnapshot, sessionID, "", "", protocol.RestoreSnapshotData{
				SnapshotID: snapID,
			})
			continue
		}

		turnID := fmt.Sprintf("turn-%d", atomic.AddUint64(&turnCounter, 1))
		_ = clientStream.SendEvent(protocol.EventUserPrompt, sessionID, turnID, "", protocol.UserPromptData{
			Prompt:    line,
			SessionID: sessionID,
		})
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading stdin: %v\n", err)
	}

	_ = cmd.Wait()
}
