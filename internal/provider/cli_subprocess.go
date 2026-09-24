package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// CLIKind names one coding-agent CLI yoakeland can drive (SPEC.md §6b).
type CLIKind string

const (
	CLIKindClaude   CLIKind = "claude"
	CLIKindCodex    CLIKind = "codex"
	CLIKindCursor   CLIKind = "cursor"
	CLIKindKimi     CLIKind = "kimi"
	CLIKindOpenCode CLIKind = "opencode"
	CLIKindGemini   CLIKind = "gemini"
	CLIKindGrok     CLIKind = "grok"
	CLIKindDeepSeek CLIKind = "deepseek"
)

// cliSpec is how to launch one CLI and which wire protocol it speaks:
// Claude Code's stream-json, Codex's app-server, or the Agent Client
// Protocol that every other agent here implements.
type cliSpec struct {
	Binary   string
	Args     []string // ACP agents only; claude/codex build their own
	Protocol string   // "claude" | "codex" | "acp"
}

var cliSpecs = map[CLIKind]cliSpec{
	CLIKindClaude: {Binary: "claude", Protocol: "claude"},
	CLIKindCodex:  {Binary: "codex", Protocol: "codex"},
	// Cursor documents `agent` as its command, but that name is generic
	// enough to collide with other tools; `cursor-agent` is its own alias.
	CLIKindCursor:   {Binary: "cursor-agent", Args: []string{"acp"}, Protocol: "acp"},
	CLIKindKimi:     {Binary: "kimi", Args: []string{"acp"}, Protocol: "acp"},
	CLIKindOpenCode: {Binary: "opencode", Args: []string{"acp"}, Protocol: "acp"},
	CLIKindGemini:   {Binary: "gemini", Args: []string{"--acp"}, Protocol: "acp"},
	CLIKindGrok:     {Binary: "grok", Args: []string{"agent", "stdio"}, Protocol: "acp"},
	CLIKindDeepSeek: {Binary: "dsh", Args: []string{"--profile", "acp"}, Protocol: "acp"},
}

// CLIKinds lists every supported CLI, for detect_cli.
func CLIKinds() []CLIKind {
	return []CLIKind{CLIKindClaude, CLIKindCodex, CLIKindCursor, CLIKindKimi, CLIKindOpenCode, CLIKindGemini, CLIKindGrok, CLIKindDeepSeek}
}

// DetectCLI reports whether kind's binary is on PATH, so a frontend can show
// which subscription agents this machine has before the user registers one.
func DetectCLI(kind CLIKind) bool {
	spec, ok := cliSpecs[kind]
	if !ok {
		return false
	}
	_, err := exec.LookPath(spec.Binary)
	return err == nil
}

// CLISubprocessProvider drives a coding-agent CLI (claude, codex, or any ACP
// agent) as the whole agent, not as a text model: it runs the agent's own
// tools, sandbox and session, and relays what the agent reports — streamed
// text and reasoning, tool calls and their results, permission asks,
// questions, usage — into yoakeland's turn loop as an agent.NativeAgent.
// Usage is billed to the user's own subscription. One CLI process per turn;
// continuity comes from the agent's own session (agent.NativeTurn.ResumeID).
type CLISubprocessProvider struct {
	Kind  CLIKind
	Model string
	Dir   string

	// OnRateLimits, if set, receives the subscription usage the CLI reports
	// (SPEC.md §2a ServerStatus.rateLimits).
	OnRateLimits func(protocol.ProviderRateLimits)

	// Start overrides process creation (tests); nil runs the real binary.
	Start procStarter

	effort string
}

func (p *CLISubprocessProvider) ModelName() string { return p.Model }

func (p *CLISubprocessProvider) RunsOwnTools() {}

// SetModelOptions takes the reasoning effort (claude --effort, codex's turn
// effort, an ACP agent's thought_level option). ThinkingBudget has no CLI
// equivalent.
func (p *CLISubprocessProvider) SetModelOptions(opts agent.ModelOptions) {
	if opts.ReasoningEffort != "" {
		p.effort = opts.ReasoningEffort
	}
}

func (p *CLISubprocessProvider) start() procStarter {
	if p.Start != nil {
		return p.Start
	}
	return startProc
}

func (p *CLISubprocessProvider) StreamTurn(ctx context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 16)
	eChan := make(chan error, 1)
	go func() {
		defer close(cChan)
		defer close(eChan)
		nt := agent.NativeTurnFromContext(ctx)
		spec, ok := cliSpecs[p.Kind]
		if nt == nil || !ok {
			eChan <- fmt.Errorf("CLI agent %q can't run outside a native turn", p.Kind)
			return
		}
		// Every CLI process dies with the turn, however the turn ends.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		emit := func(c agent.ModelStreamChunk) {
			select {
			case cChan <- c:
			case <-ctx.Done():
			}
		}
		var err error
		switch spec.Protocol {
		case "claude":
			err = p.runClaude(ctx, nt, emit)
		case "codex":
			err = p.runCodex(ctx, nt, emit)
		default:
			err = p.runACP(ctx, spec, nt, emit)
		}
		if err != nil {
			eChan <- err
		}
	}()
	return cChan, eChan
}

func (p *CLISubprocessProvider) reportRateLimits(rl *protocol.ProviderRateLimits) {
	if rl != nil && p.OnRateLimits != nil && (rl.FiveHour != nil || rl.SevenDay != nil || len(rl.Other) > 0) {
		p.OnRateLimits(*rl)
	}
}

func unixRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func roundPct(x float64) float64 { return math.Round(x*100) / 100 }

type codexWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

// parseCodexRateLimits maps Codex's primary/secondary windows (a
// RateLimitSnapshot) onto the fiveHour/sevenDay slots by their length; any
// other length (a free plan's 30-day window) goes in Other.
func parseCodexRateLimits(raw []byte) (*protocol.ProviderRateLimits, error) {
	var snap struct {
		Primary   *codexWindow `json:"primary"`
		Secondary *codexWindow `json:"secondary"`
		PlanType  *string      `json:"planType"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parsing codex rate limits: %w", err)
	}
	rl := &protocol.ProviderRateLimits{}
	if snap.PlanType != nil {
		rl.Plan = *snap.PlanType
	}
	for _, w := range []*codexWindow{snap.Primary, snap.Secondary} {
		if w == nil {
			continue
		}
		win := protocol.RateLimitWindow{UsedPct: roundPct(w.UsedPercent), ResetsAt: unixRFC3339(w.ResetsAt)}
		switch w.WindowDurationMins {
		case 300:
			rl.FiveHour = &win
		case 7 * 24 * 60:
			rl.SevenDay = &win
		default:
			rl.Other = append(rl.Other, protocol.OtherRateLimitWindow{WindowMins: w.WindowDurationMins, RateLimitWindow: win})
		}
	}
	return rl, nil
}
