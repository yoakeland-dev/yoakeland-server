package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/terminal"
)

// The terminal tools run commands in real, persistent shells — the same tabs
// a user opens in their terminal panel, not the bash tool's one-shot,
// no-shell exec. A process started here (a dev server, a watcher) outlives
// the tool call and the turn, and the user sees and can type into it.

const (
	maxTerminalWait = 120 * time.Second
	// maxTerminalOutput is how much of a read's tail the model sees.
	maxTerminalOutput = 12000
)

func clampWait(ms int, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return min(time.Duration(ms)*time.Millisecond, maxTerminalWait)
}

// waitTerminal polls until a command that was running (or about to be)
// finishes, the shell exits, wait elapses, or ctx ends.
func waitTerminal(ctx context.Context, m *terminal.Manager, sessionID, id string, finishedBefore int, wait time.Duration) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		st, err := m.State(sessionID, id)
		if err != nil || st.Status != protocol.TerminalStatusRunning || st.Finished > finishedBefore {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// report renders a terminal's new output and state for the model.
func reportTerminal(m *terminal.Manager, sessionID, id string, finishedBefore int) (string, error) {
	out, skipped, st, err := m.Read(sessionID, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "terminal %s", st.ID)
	if st.Name != "" {
		fmt.Fprintf(&b, " (%s)", st.Name)
	}
	switch {
	case st.Status != protocol.TerminalStatusRunning:
		b.WriteString(": the shell exited")
		if st.ExitCode != nil {
			fmt.Fprintf(&b, " with code %d", *st.ExitCode)
		}
	case st.Finished > finishedBefore:
		b.WriteString(": command finished")
		if st.LastExit != nil {
			fmt.Fprintf(&b, " with exit code %d", *st.LastExit)
		}
	case st.Busy:
		b.WriteString(": command still running")
	default:
		b.WriteString(": running")
	}
	b.WriteString("\n")
	text := cleanTerminalOutput(out)
	if skipped > 0 || len(text) > maxTerminalOutput {
		if len(text) > maxTerminalOutput {
			text = text[len(text)-maxTerminalOutput:]
		}
		b.WriteString("[earlier output omitted]\n")
	}
	if strings.TrimSpace(text) == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString(text)
	}
	return b.String(), nil
}

// Escape sequences: OSC (…BEL or …ST), CSI, and two-byte ESC forms.
var ansiRE = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)

// cleanTerminalOutput strips escape sequences and applies carriage-return
// overwrites, so a progress bar reads as its final state, not every frame.
func cleanTerminalOutput(raw []byte) string {
	s := ansiRE.ReplaceAllString(string(raw), "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

type TerminalStartTool struct{ m *terminal.Manager }

func NewTerminalStartTool(m *terminal.Manager) *TerminalStartTool { return &TerminalStartTool{m} }

func (t *TerminalStartTool) Name() string { return "terminal_start" }
func (t *TerminalStartTool) Description() string {
	return "Start a command in a new persistent terminal: a real interactive shell (pipes, &&, env vars all work) " +
		"that keeps running after this call returns, shown to the user as a tab in their terminal panel. " +
		"Use it for long-lived processes (dev servers, watchers) and long builds or test runs; prefer bash for quick one-off commands. " +
		"Returns the terminal id and its output once the command finishes or wait_ms passes, whichever is first. " +
		"Read more output (or type into it) with terminal_output; stop it with terminal_stop."
}
func (t *TerminalStartTool) RequiresPermission(_ []byte) bool { return true }
func (t *TerminalStartTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: t.Description(), Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Command line to run in the shell, e.g. \"npm run dev\"."},
			"name":    map[string]any{"type": "string", "description": "Short tab name the user sees, e.g. \"dev server\"."},
			"cwd":     map[string]any{"type": "string", "description": "Working directory relative to the project root. Defaults to the root."},
			"wait_ms": map[string]any{"type": "integer", "description": "How long to wait for output before returning (default 5000, max 120000). The command keeps running either way."},
		},
		"required": []string{"command", "name"},
	}}
}

func (t *TerminalStartTool) Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args struct {
		Command string `json:"command"`
		Name    string `json:"name"`
		Cwd     string `json:"cwd"`
		WaitMs  int    `json:"wait_ms"`
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	// The terminal must outlive this turn, so it can't inherit its cancel.
	id, err := t.m.Start(context.WithoutCancel(ctx), execCtx.SessionID, args.Name, args.Cwd, args.Command)
	if err != nil {
		return "", err
	}
	waitTerminal(ctx, t.m, execCtx.SessionID, id, 0, clampWait(args.WaitMs, 5*time.Second))
	return reportTerminal(t.m, execCtx.SessionID, id, 0)
}

type TerminalOutputTool struct{ m *terminal.Manager }

func NewTerminalOutputTool(m *terminal.Manager) *TerminalOutputTool { return &TerminalOutputTool{m} }

func (t *TerminalOutputTool) Name() string { return "terminal_output" }
func (t *TerminalOutputTool) Description() string {
	return "Read what a terminal printed since you last read it, optionally typing input into it first. " +
		"Works on any terminal in this chat, including ones the user opened (omit id to list them). " +
		"input is sent as keystrokes: end it with \\n to press Enter, send \\u0003 for Ctrl-C. " +
		"With input, waits for the command it runs to finish or wait_ms (default 3000), whichever is first; without input, returns immediately unless wait_ms is set."
}
func (t *TerminalOutputTool) RequiresPermission(argsJSON []byte) bool {
	var args struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(argsJSON, &args)
	return args.Input != ""
}
func (t *TerminalOutputTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: t.Description(), Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":      map[string]any{"type": "string", "description": "Terminal id, e.g. \"agent-1\". Omit to list this chat's terminals."},
			"input":   map[string]any{"type": "string", "description": "Keystrokes to type before reading."},
			"wait_ms": map[string]any{"type": "integer", "description": "How long to wait for more output (max 120000)."},
		},
	}}
}

func (t *TerminalOutputTool) Execute(ctx context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args struct {
		ID     string `json:"id"`
		Input  string `json:"input"`
		WaitMs int    `json:"wait_ms"`
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return listTerminals(t.m, execCtx.SessionID), nil
	}
	st, err := t.m.State(execCtx.SessionID, args.ID)
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, listTerminals(t.m, execCtx.SessionID))
	}
	wait := time.Duration(0)
	if args.Input != "" {
		if err := t.m.Write(execCtx.SessionID, args.ID, []byte(args.Input)); err != nil {
			return "", err
		}
		wait = 3 * time.Second
	}
	waitTerminal(ctx, t.m, execCtx.SessionID, args.ID, st.Finished, clampWait(args.WaitMs, wait))
	return reportTerminal(t.m, execCtx.SessionID, args.ID, st.Finished)
}

func listTerminals(m *terminal.Manager, sessionID string) string {
	list := m.List(sessionID)
	if len(list) == 0 {
		return "No terminals are open in this chat. Start one with terminal_start."
	}
	var b strings.Builder
	b.WriteString("Terminals in this chat:\n")
	for _, s := range list {
		name := s.Name
		if name == "" {
			name = s.Label
		}
		who := "user"
		if s.Agent {
			who = "yours"
		}
		fmt.Fprintf(&b, "- %s (%s, %s): %s\n", s.TerminalID, name, who, s.Status)
	}
	return b.String()
}

type TerminalStopTool struct{ m *terminal.Manager }

func NewTerminalStopTool(m *terminal.Manager) *TerminalStopTool { return &TerminalStopTool{m} }

func (t *TerminalStopTool) Name() string { return "terminal_stop" }
func (t *TerminalStopTool) Description() string {
	return "Stop a terminal you started with terminal_start: kills its process and closes the tab. Returns its last output."
}
func (t *TerminalStopTool) RequiresPermission(_ []byte) bool { return false }
func (t *TerminalStopTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.Name(), Description: t.Description(), Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string", "description": "Terminal id, e.g. \"agent-1\"."}},
		"required":   []string{"id"},
	}}
}

func (t *TerminalStopTool) Execute(_ context.Context, execCtx ExecutionContext, argsJSON []byte) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	st, err := t.m.State(execCtx.SessionID, args.ID)
	if err != nil {
		return "", err
	}
	// The user's own tabs are theirs to close; the agent only cleans up after itself.
	if !st.Agent {
		return "", fmt.Errorf("terminal %s was opened by the user, not you; ask them to close it", args.ID)
	}
	last, _ := reportTerminal(t.m, execCtx.SessionID, args.ID, st.Finished)
	if err := t.m.Close(execCtx.SessionID, args.ID); err != nil {
		return "", err
	}
	return "Stopped. " + last, nil
}
