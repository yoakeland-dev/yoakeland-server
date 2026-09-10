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
	"sync"
	"sync/atomic"
	"yoakeland/internal/protocol"
)

type pendingPerm struct {
	SessionID  string
	TurnID     string
	ToolCallID string
	ToolName   string
	Desc       string
}

func parseCommandArgs(cmdStr string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	quoteChar := rune(0)

	for _, r := range cmdStr {
		if inQuote {
			if r == quoteChar {
				inQuote = false
			} else {
				current.WriteRune(r)
			}
		} else {
			switch r {
			case '\'', '"':
				inQuote = true
				quoteChar = r
			case ' ', '\t':
				if current.Len() > 0 {
					args = append(args, current.String())
					current.Reset()
				}
			default:
				current.WriteRune(r)
			}
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

func main() {
	var backendCmdStr string
	var sessionID string
	flag.StringVar(&backendCmdStr, "bin", "./yoakeland", "Path or command line for yoakeland backend binary")
	flag.StringVar(&sessionID, "session", "session-main", "Session ID to join or resume")
	flag.Parse()

	cmdParts := parseCommandArgs(backendCmdStr)
	if len(cmdParts) == 0 {
		fmt.Fprintf(os.Stderr, "no backend binary specified\n")
		os.Exit(1)
	}

	cmd := exec.Command(cmdParts[0], cmdParts[1:]...)
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

	var permMu sync.Mutex
	var activePermReq *pendingPerm

	// Background listener for events from backend
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

				permMu.Lock()
				activePermReq = &pendingPerm{
					SessionID:  env.SessionID,
					TurnID:     env.TurnID,
					ToolCallID: env.ToolCallID,
					ToolName:   d.ToolName,
					Desc:       d.Description,
				}
				permMu.Unlock()

				fmt.Printf("\n[PERMISSION REQUIRED] %s (%s)\nAllow? (y/n, append 'r' to remember forever, e.g. 'y' or 'yr'): ", d.ToolName, d.Description)

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

	// Single Stdin Scanner Loop (Sole Owner of os.Stdin)
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("yoakeland client connected [Session: %s]\nType a prompt or 'exit':\n> ", sessionID)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			permMu.Lock()
			hasPerm := activePermReq != nil
			permMu.Unlock()

			if hasPerm {
				fmt.Print("Please allow or deny: (y/n/yr/nr): ")
			} else {
				fmt.Print("> ")
			}
			continue
		}

		if line == "exit" || line == "quit" {
			_ = cmd.Process.Kill()
			break
		}

		// Check if we are answering a pending permission request
		permMu.Lock()
		req := activePermReq
		activePermReq = nil
		permMu.Unlock()

		if req != nil {
			lower := strings.ToLower(line)
			allowed := strings.HasPrefix(lower, "y")
			remember := strings.Contains(lower, "r")

			_ = clientStream.SendEvent(
				protocol.EventPermissionResponse,
				req.SessionID,
				req.TurnID,
				req.ToolCallID,
				protocol.PermissionResponseData{
					Allowed:  allowed,
					Remember: remember,
				},
			)
			continue
		}

		// Handle Snapshot restoration command
		if strings.HasPrefix(line, "restore ") {
			snapID := strings.TrimSpace(strings.TrimPrefix(line, "restore "))
			_ = clientStream.SendEvent(protocol.EventRestoreSnapshot, sessionID, "", "", protocol.RestoreSnapshotData{
				SnapshotID: snapID,
			})
			continue
		}

		// Standard user prompt
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
