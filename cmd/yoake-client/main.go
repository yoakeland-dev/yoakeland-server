package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
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
	"yoakeland/internal/sockpath"
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	saved := loadClientConfig()

	var backendCmdStr string
	var sessionID string
	var connect string
	var token string
	var caPath string
	var name string
	flag.StringVar(&backendCmdStr, "bin", "./yoakeland", "Path or command line of the yoakeland binary (spawn mode; ignored with -connect)")
	flag.StringVar(&sessionID, "session", firstNonEmpty(saved.Session, "session-main"), "Session ID to join or resume")
	flag.StringVar(&connect, "connect", saved.Connect, "Connect to a running yoakeland server instead of spawning one: 'local' for the local unix socket (<state-dir>/yoakeland.sock), a unix socket path, or 'tcp://host:port' for the remote TLS+token listener (-token/-ca). Unset means spawn mode via -bin.")
	flag.StringVar(&token, "token", saved.Token, "Auth token for a tcp:// -connect target (SPEC.md §2a).")
	flag.StringVar(&caPath, "ca", saved.CA, "Path to the server's tls/ca.pem for a tcp:// -connect target, copied over out of band. Omit to trust the system cert pool (for a publicly trusted server certificate).")
	flag.StringVar(&name, "name", saved.Name, "Self-reported peer name for a tcp:// -connect target. Defaults to the hostname.")
	flag.Parse()

	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}

	// Remember this run's settings so the next invocation on this device
	// doesn't need to repeat them.
	_ = saveClientConfig(ClientConfig{Connect: connect, Token: token, CA: caPath, Name: name, Session: sessionID})

	var clientStream *protocol.IPCStream
	var cmd *exec.Cmd
	var childStdin io.WriteCloser

	if strings.HasPrefix(connect, "tcp://") {
		addr := strings.TrimPrefix(connect, "tcp://")
		tlsConfig := &tls.Config{}
		if caPath != "" {
			caPEM, err := os.ReadFile(caPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read -ca %s: %v\n", caPath, err)
				os.Exit(1)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				fmt.Fprintf(os.Stderr, "no valid certificates found in -ca %s\n", caPath)
				os.Exit(1)
			}
			tlsConfig.RootCAs = pool
		}
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to %s: %v\n", addr, err)
			os.Exit(1)
		}
		defer conn.Close()
		clientStream = protocol.NewIPCStream(conn, conn)

		// TCP requires an auth envelope as the very first frame (SPEC.md §2a).
		if err := clientStream.SendEvent(protocol.EventAuth, "", "", "", protocol.AuthData{
			Token:      token,
			Name:       name,
			DeviceType: "cli",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "failed to send auth: %v\n", err)
			os.Exit(1)
		}
	} else if connect != "" {
		socketPath := connect
		if connect == "local" {
			socketPath = sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(sockpath.DefaultStateDir()))
		}
		conn, err := sockpath.Dial(socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to %s: %v\n", socketPath, err)
			os.Exit(1)
		}
		defer conn.Close()
		// A unix socket needs no auth handshake (SPEC.md §2a) — the first
		// envelope can be a user_prompt directly, exactly like stdio mode.
		clientStream = protocol.NewIPCStream(conn, conn)
	} else {
		cmdParts := parseCommandArgs(backendCmdStr)
		if len(cmdParts) == 0 {
			fmt.Fprintf(os.Stderr, "no backend binary specified\n")
			os.Exit(1)
		}
		// yoakeland defaults to daemon mode (always-on local socket, exits
		// only on SIGINT/SIGTERM) — this spawn-and-pipe path needs the
		// opposite contract (speak stdio directly, exit on stdin EOF), so
		// request it explicitly rather than relying on the backend's own
		// default, which this client does not control.
		cmdParts = append(cmdParts, "-stdio")

		cmd = exec.Command(cmdParts[0], cmdParts[1:]...)
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

		childStdin = stdinPipe
		clientStream = protocol.NewIPCStream(stdoutPipe, stdinPipe)
	}

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
			case protocol.EventSessionHistory:
				var d protocol.SessionHistoryData
				_ = json.Unmarshal(env.Data, &d)
				if len(d.Messages) > 0 {
					fmt.Printf("[client] Resuming session %s (%d prior messages):\n", d.SessionID, len(d.Messages))
					for _, m := range d.Messages {
						if m.Content != "" {
							role := m.Role
							if m.Author != "" {
								role = fmt.Sprintf("%s (%s)", m.Role, m.Author)
							}
							fmt.Printf("  %s: %s\n", role, m.Content)
						}
					}
				}
				fmt.Print("> ")

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

				fmt.Printf("\n[PERMISSION REQUIRED] %s (%s)\nAllow? y/n, add r to remember forever (e.g. yr): ", d.ToolName, d.Description)

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
	fmt.Printf("yoakeland client connected [Session: %s]\nType a prompt or 'exit':\n", sessionID)

	// Ask the backend to replay this session's persisted history (if any) —
	// the server is the source of truth for a session's transcript (see
	// SPEC.md §6, get_session_history), not this client. The background
	// listener above prints it (and the next "> " prompt) once it arrives.
	_ = clientStream.SendEvent(protocol.EventGetSessionHistory, sessionID, "", "", protocol.GetSessionHistoryData{
		SessionID: sessionID,
	})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			permMu.Lock()
			hasPerm := activePermReq != nil
			permMu.Unlock()

			if hasPerm {
				fmt.Print("Allow or deny? (y/n/yr/nr): ")
			} else {
				fmt.Print("> ")
			}
			continue
		}

		if line == "exit" || line == "quit" {
			if cmd != nil {
				_ = cmd.Process.Kill()
			}
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

	if cmd != nil {
		// The child exits on stdin EOF, so close our end before Wait or
		// both sides wait on each other.
		_ = childStdin.Close()
		_ = cmd.Wait()
	}
}
