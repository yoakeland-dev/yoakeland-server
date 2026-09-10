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
	flag.StringVar(&backendBin, "bin", "./yoakeland", "Path to yoakeland backend binary")
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
		fmt.Fprintf(os.Stderr, "failed to start backend process: %v\n", err)
		os.Exit(1)
	}

	clientStream := protocol.NewIPCStream(stdoutPipe, stdinPipe)
	var turnCounter uint64

	// Background listener for events from backend
	go func() {
		for {
			env, err := clientStream.ReadEnvelope()
			if err != nil {
				if err == io.EOF {
					fmt.Println("\n[client] Backend closed IPC channel.")
					os.Exit(0)
				}
				fmt.Fprintf(os.Stderr, "\n[client] Error reading line: %v\n", err)
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
				fmt.Printf("\n[TOOL CALL] %s (id: %s) -> Args: %s\n", d.Name, env.ToolCallID, string(d.Args))

			case protocol.EventPermissionRequest:
				var d protocol.PermissionRequestData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[PERMISSION REQUIRED] %s (%s)\nAllow? (y/n): ", d.ToolName, d.Description)

				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				allow := strings.TrimSpace(strings.ToLower(line)) == "y"

				_ = clientStream.SendEvent(
					protocol.EventPermissionResponse,
					env.TurnID,
					env.ToolCallID,
					protocol.PermissionResponseData{Allowed: allow, Remember: false},
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
				fmt.Printf("\n--- [Turn completed: %s] ---\n> ", env.TurnID)

			case protocol.EventError:
				var d protocol.ErrorData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[BACKEND ERROR] %s\n> ", d.Message)
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("yoakeland test client initialized. Type a prompt:")
	fmt.Print("> ")

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

		turnID := fmt.Sprintf("turn-%d", atomic.AddUint64(&turnCounter, 1))
		err := clientStream.SendEvent(protocol.EventUserPrompt, turnID, "", protocol.UserPromptData{
			Prompt: line,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to send prompt: %v\n", err)
			break
		}
	}

	_ = cmd.Wait()
}
