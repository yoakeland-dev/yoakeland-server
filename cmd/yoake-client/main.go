package main

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sockpath"
)

// pendingAsk is something the backend is blocked on: a permission request
// (allow/deny) or an ask_user_question. They are answered oldest first.
type pendingAsk struct {
	SessionID  string
	TurnID     string
	ToolCallID string
	Perm       bool     // a permission request rather than a question
	Options    []string // a question's options, if it has any
}

// sanitize drops control characters from text the backend or the model
// produced, so a repo's files can't clear the screen, hide the permission
// prompt or write the clipboard through escape sequences. Newlines and tabs
// are kept.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// parsePermissionAnswer accepts only the answers the prompt names. A loose
// match would let a stray word allow a tool, or save a rule that lasts forever.
func parsePermissionAnswer(line string) (allowed, remember, ok bool) {
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, false, true
	case "yr", "always":
		return true, true, true
	case "n", "no":
		return false, false, true
	case "nr":
		return false, true, true
	}
	return false, false, false
}

// backendPath resolves a bare binary name from PATH, then from beside this
// client, never from the working directory: a repo could ship its own
// yoakeland there. An explicit path is returned as is.
func backendPath(name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return name
}

// newDeviceID makes the stable id this client presents on every TCP connect,
// so the server recognises the device across reconnects (SPEC.md §2a).
func newDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
	flag.StringVar(&backendCmdStr, "bin", "yoakeland", "Command line of the yoakeland binary (spawn mode; ignored with -connect). A bare name is found on PATH, then beside this client; a path such as ./yoakeland runs that file.")
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

	deviceID := saved.DeviceID
	if deviceID == "" {
		deviceID = newDeviceID()
	}

	// Remember this run's settings so the next invocation on this device
	// doesn't need to repeat them.
	_ = saveClientConfig(ClientConfig{Connect: connect, Token: token, CA: caPath, Name: name, Session: sessionID, DeviceID: deviceID})

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
			DeviceID:   deviceID,
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
		cmdParts[0] = backendPath(cmdParts[0])
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

	// quitting is set once the user has asked to leave, so the backend closing
	// its end after that is expected rather than a failure.
	var quitting atomic.Bool

	var permMu sync.Mutex
	var pending []pendingAsk // oldest first; the next answer goes to pending[0]

	// check reports a send that failed, so the user knows an answer or prompt
	// never reached the backend.
	check := func(err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "[client] send failed: %v\n", err)
		}
	}

	// Background listener for events from backend
	go func() {
		for {
			env, err := clientStream.ReadEnvelope()
			if err != nil {
				if quitting.Load() {
					return
				}
				// The backend went away underneath us. Say so and fail, rather
				// than leaving the user typing into a dead stream.
				if err == io.EOF {
					fmt.Fprintln(os.Stderr, "\n[client] Backend closed.")
				} else {
					fmt.Fprintf(os.Stderr, "\n[client] Read from backend failed: %v\n", err)
				}
				os.Exit(1)
			}

			switch env.Type {
			case protocol.EventPing:
				// Idle connections are dropped if they never answer a ping (SPEC.md §2a).
				var d protocol.PingData
				_ = json.Unmarshal(env.Data, &d)
				check(clientStream.SendEvent(protocol.EventPong, "", "", "", protocol.PongData{Nonce: d.Nonce}))

			case protocol.EventPairingPending:
				fmt.Printf("\n[client] Waiting for someone at the desktop to approve this device...\n")

			case protocol.EventSessionHistory:
				var d protocol.SessionHistoryData
				_ = json.Unmarshal(env.Data, &d)
				if len(d.Messages) > 0 {
					fmt.Printf("[client] Resuming session %s (%d prior messages):\n", sanitize(d.SessionID), len(d.Messages))
					for _, m := range d.Messages {
						if m.Content != "" {
							role := sanitize(m.Role)
							if m.Author != "" {
								role = fmt.Sprintf("%s (%s)", role, sanitize(m.Author))
							}
							fmt.Printf("  %s: %s\n", role, sanitize(m.Content))
						}
					}
				}
				fmt.Print("> ")

			case protocol.EventModelDelta:
				var d protocol.ModelDeltaData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Print(sanitize(d.Content))

			case protocol.EventToolCallRequested:
				var d protocol.ToolCallData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[TOOL CALL] %s -> %s\n", sanitize(d.Name), sanitize(string(d.Args)))

			case protocol.EventPermissionRequest:
				var d protocol.PermissionRequestData
				_ = json.Unmarshal(env.Data, &d)

				permMu.Lock()
				pending = append(pending, pendingAsk{SessionID: env.SessionID, TurnID: env.TurnID, ToolCallID: env.ToolCallID, Perm: true})
				permMu.Unlock()

				// A plan ask is the agent leaving plan mode. This client has no
				// plan view, so it is answered as an ordinary allow or deny.
				label := "PERMISSION REQUIRED"
				if d.Plan {
					label = "PLAN APPROVAL REQUIRED"
				}
				fmt.Printf("\n[%s] %s (%s)\nAllow? y/n, add r to remember forever (e.g. yr): ", label, sanitize(d.ToolName), sanitize(d.Description))

			case protocol.EventAskUserQuestionRequested:
				var d protocol.AskUserQuestionRequestedData
				_ = json.Unmarshal(env.Data, &d)

				permMu.Lock()
				pending = append(pending, pendingAsk{SessionID: env.SessionID, TurnID: env.TurnID, ToolCallID: env.ToolCallID, Options: d.Options})
				permMu.Unlock()

				label := "QUESTION"
				if d.Total > 1 {
					label = fmt.Sprintf("QUESTION %d of %d", d.Index, d.Total)
				}
				fmt.Printf("\n[%s] %s\n", label, sanitize(d.Question))
				for i, o := range d.Options {
					fmt.Printf("  %d) %s\n", i+1, sanitize(o))
				}
				fmt.Print("Answer: ")

			case protocol.EventToolResult:
				var d protocol.ToolResultData
				_ = json.Unmarshal(env.Data, &d)
				if d.Error != "" {
					fmt.Printf("[TOOL ERR] %s: %s\n", sanitize(d.ToolName), sanitize(d.Error))
				} else {
					fmt.Printf("[TOOL OUT] %s:\n%s\n", sanitize(d.ToolName), sanitize(d.Output))
				}

			case protocol.EventTurnCompleted:
				fmt.Printf("\n--- [Turn completed (Session: %s)] ---\n> ", sanitize(env.SessionID))

			case protocol.EventSnapshotRestored:
				var d protocol.SnapshotEventData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[SNAPSHOT RESTORED] Snapshot ID: %s\n> ", sanitize(d.SnapshotID))

			case protocol.EventError:
				var d protocol.ErrorData
				_ = json.Unmarshal(env.Data, &d)
				fmt.Printf("\n[BACKEND ERROR] %s\n> ", sanitize(d.Message))
			}
		}
	}()

	// Single Stdin Scanner Loop (Sole Owner of os.Stdin)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), protocol.MaxFrameSize)
	fmt.Printf("yoakeland client connected [Session: %s]\nType a prompt or 'exit':\n", sanitize(sessionID))

	// Ask the backend to replay this session's persisted history (if any) —
	// the server is the source of truth for a session's transcript (see
	// SPEC.md §6, get_session_history), not this client. The background
	// listener above prints it (and the next "> " prompt) once it arrives.
	check(clientStream.SendEvent(protocol.EventGetSessionHistory, sessionID, "", "", protocol.GetSessionHistoryData{
		SessionID: sessionID,
	}))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		permMu.Lock()
		hasAsk := len(pending) > 0
		var head pendingAsk
		if hasAsk {
			head = pending[0]
		}
		permMu.Unlock()

		if line == "" {
			switch {
			case hasAsk && head.Perm:
				fmt.Print("Allow or deny? (y/n/yr/nr): ")
			case hasAsk:
				fmt.Print("Answer: ")
			default:
				fmt.Print("> ")
			}
			continue
		}

		if line == "exit" || line == "quit" {
			quitting.Store(true)
			if cmd != nil {
				_ = cmd.Process.Kill()
			}
			break
		}

		// The next line answers the oldest pending ask. Only the reader
		// goroutine appends to pending, so head is still pending[0] here.
		if hasAsk {
			if head.Perm {
				allowed, remember, ok := parsePermissionAnswer(line)
				if !ok {
					fmt.Print("Please answer y, n, yr or nr: ")
					continue
				}
				check(clientStream.SendEvent(
					protocol.EventPermissionResponse,
					head.SessionID,
					head.TurnID,
					head.ToolCallID,
					protocol.PermissionResponseData{
						Allowed:  allowed,
						Remember: remember,
					},
				))
			} else {
				// A number picks an option; anything else is the answer as typed.
				answer := line
				if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(head.Options) {
					answer = head.Options[n-1]
				}
				check(clientStream.SendEvent(protocol.EventAskUserAnswer, head.SessionID, head.TurnID, head.ToolCallID, protocol.AskUserAnswerData{
					Answer: answer,
				}))
			}
			permMu.Lock()
			pending = pending[1:]
			permMu.Unlock()
			continue
		}

		// Handle Snapshot restoration command
		if strings.HasPrefix(line, "restore ") {
			snapID := strings.TrimSpace(strings.TrimPrefix(line, "restore "))
			check(clientStream.SendEvent(protocol.EventRestoreSnapshot, sessionID, "", "", protocol.RestoreSnapshotData{
				SnapshotID: snapID,
			}))
			continue
		}

		// Standard user prompt
		turnID := fmt.Sprintf("turn-%d", atomic.AddUint64(&turnCounter, 1))
		check(clientStream.SendEvent(protocol.EventUserPrompt, sessionID, turnID, "", protocol.UserPromptData{
			Prompt:    line,
			SessionID: sessionID,
		}))
	}

	quitting.Store(true)
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
