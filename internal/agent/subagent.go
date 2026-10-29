package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/tool"
)

// Subagents for providers with no subagents of their own (the plain API
// ones; a CLI agent like Claude Code runs its own Task tool instead, whose
// steps arrive tagged with the Task's id — see native_claude.go). The model
// calls the subagent tool; each call runs a child Agent — fresh history, the
// same provider and tools minus subagent itself — to completion inside the
// parent's turn, and its final message becomes the call's result. Calls in
// one response run in parallel. Everything a child sends reaches the
// session's subscribers tagged with the call's id (protocol.StreamSet.
// WithParent); its permission and question asks surface like the parent's
// own. Its transcript is kept on the call's tool message (Message.Subagent),
// never in the parent's model context.

const SubagentToolName = "subagent"

var subagentDefinition = tool.ToolDefinition{
	Name: SubagentToolName,
	Description: "Hand a self-contained task to a subagent: a fresh agent with your tools (except this one) that sees none of " +
		"this conversation, works on its own, and returns only its final report. Use it for independent chunks of work that " +
		"would otherwise fill your context, like investigating how something works across the codebase, or making several " +
		"unrelated changes. Subagent calls made in the same response run at the same time. The prompt must carry everything " +
		"the subagent needs: the goal, relevant paths, constraints, and what to report back. Use kind \"explore\" for read-only " +
		"investigation (it runs on a faster, cheaper model and can't change anything) and \"general\" for anything that needs " +
		"judgment or edits.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description": map[string]any{"type": "string", "description": "3-6 word summary the user sees, e.g. \"Map the auth flow\"."},
			"prompt":      map[string]any{"type": "string", "description": "The complete task for the subagent."},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"explore", "general"},
				"description": "\"explore\": read-only search and investigation. \"general\" (default): anything else.",
			},
		},
		"required": []string{"description", "prompt"},
	},
}

type subagentResult struct {
	output, errText string
	usage           protocol.UsageData
	transcript      []Message
}

// spendTally is the cost a round's subagents are checked against: what the
// session had spent when they started, plus what each has priced since. They
// run at once, so checking each one's own spend alone would let N of them
// together pass a cap that none of them alone breaks.
type spendTally struct {
	mu  sync.Mutex
	usd float64
}

func (t *spendTally) add(usd float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usd += usd
}

func (t *spendTally) total() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usd
}

// startSubagents launches every subagent call in calls the current mode
// allows, each in its own goroutine, keyed by call id. Each channel gets
// exactly one result. spentThisTurn is the turn's priced cost so far, which
// the subagents' spend tally starts from.
func (a *Agent) startSubagents(ctx context.Context, turnID string, author Author, bound RoleBinding, calls []ToolCall, spentThisTurn float64) map[string]chan subagentResult {
	var out map[string]chan subagentResult
	tally := &spendTally{usd: spentThisTurn}
	if a.usageStore != nil {
		tally.usd += a.usageStore.SessionTotal(a.CurrentSessionID()).CostUSD
	}
	for _, tc := range calls {
		if tc.Name != SubagentToolName || !a.isToolAllowedInMode(tc.Name) {
			continue
		}
		if out == nil {
			out = map[string]chan subagentResult{}
		}
		ch := make(chan subagentResult, 1)
		out[tc.ID] = ch
		go func(tc ToolCall) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("subagent %s panicked: %v\n%s", tc.ID, r, debug.Stack())
					ch <- subagentResult{errText: fmt.Sprintf("subagent crashed: %v", r)}
				}
			}()
			ch <- a.runSubagent(ctx, turnID, author, bound, tc, tally)
		}(tc)
	}
	return out
}

func (a *Agent) runSubagent(ctx context.Context, turnID string, author Author, bound RoleBinding, tc ToolCall, tally *spendTally) subagentResult {
	var args struct{ Description, Prompt, Kind string }
	if err := json.Unmarshal(tc.Args, &args); err != nil || strings.TrimSpace(args.Prompt) == "" {
		return subagentResult{errText: "subagent needs a prompt"}
	}
	explore := args.Kind == "explore"
	if explore {
		bound = a.roleOr(RoleExplore, bound)
	}
	child := a.newSubagent(tc.ID, bound, explore)
	child.tally = tally
	parentCtx := ctx
	ctx, child.stopSubagent = context.WithCancel(parentCtx)
	defer child.stopSubagent()
	start := time.Now()
	child.mu.Lock()
	promptID := child.nextMessageIDLocked()
	child.history = []Message{{ID: promptID, Role: "user", Content: args.Prompt, CreatedAt: start}}
	child.mu.Unlock()

	a.mu.Lock()
	a.subagents[tc.ID] = child
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.subagents, tc.ID)
		a.mu.Unlock()
		a.hooks.asksChanged() // its unanswered asks, if any, are gone with it
	}()

	_ = child.stream.SendEvent(protocol.EventSubagentStatus, child.sessionID, turnID+"."+tc.ID, "", protocol.SubagentStatusData{
		Provider: bound.ProviderName, Model: bound.Model, StartedAt: start.UTC().Format(time.RFC3339),
	})

	// Its own turn id, so its file edits get their own snapshot and its
	// turn's end doesn't close the parent's.
	err := child.runGenerationLoop(ctx, turnID+"."+tc.ID, start, author, bound, promptID)

	child.mu.Lock()
	transcript := append([]Message(nil), child.history[1:]...)
	child.mu.Unlock()
	r := subagentResult{transcript: transcript}
	for _, m := range transcript {
		if m.Usage != nil {
			addUsage(&r.usage, *m.Usage)
			r.usage.CostUSD += m.Usage.CostUSD
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			r.output = m.Content
		}
	}
	switch {
	case parentCtx.Err() != nil:
		r.errText = "subagent was cancelled"
	case ctx.Err() != nil:
		r.errText = "subagent was stopped by the user"
	case err != nil:
		r.errText = fmt.Sprintf("subagent failed: %v", err)
	case r.output == "":
		r.errText = "subagent finished without a report"
	}
	if r.errText != "" && r.output != "" {
		r.errText += "\nIts last message:\n" + r.output
		r.output = ""
	}
	return r
}

// newSubagent builds the child Agent for subagent call parentToolCallID. An
// explore subagent, or any subagent in plan mode, is read-only; otherwise it
// runs in the parent's mode, so the same permission prompts apply to what
// it does.
func (a *Agent) newSubagent(parentToolCallID string, bound RoleBinding, explore bool) *Agent {
	a.mu.Lock()
	mode, web, sessionID, roles, root := a.mode, a.webToolsEnabled, a.sessionID, a.roles, a.projectRoot
	shellOff, limits := a.shellDisabled, a.limits
	src, skillSrc := a.source, a.skillSrc
	a.mu.Unlock()
	child := NewAgent(nil, bound.Provider, bound.ProviderName, a.toolList, a.permEngine, a.snapshotStore, nil, &ContextManager{
		MaxHistoryTokens:    a.contextManager.GetMaxHistoryTokens(),
		MaxToolOutputTokens: a.contextManager.MaxToolOutputTokens,
	}, nil)
	child.stream = a.stream.WithParent(parentToolCallID)
	child.sessionID = sessionID
	child.hooks = a.hooks
	child.webToolsEnabled = web
	child.shellDisabled, child.limits = shellOff, limits
	child.roles, child.projectRoot = roles, root
	child.source, child.skillSrc = src, skillSrc
	if mode == ModePlan || explore {
		mode = ModeReadOnly
	}
	child.mode = mode
	child.systemPrompt = SubagentSystemPrompt
	child.dropTools(SubagentToolName, "write_plan")
	return child
}

// dropTools withholds names from this Agent entirely.
func (a *Agent) dropTools(names ...string) {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
		delete(a.tools, n)
	}
	filter := func(defs []tool.ToolDefinition) []tool.ToolDefinition {
		out := defs[:0:0]
		for _, d := range defs {
			if !drop[d.Name] {
				out = append(out, d)
			}
		}
		return out
	}
	a.toolDefs, a.toolDefsReadOnly, a.toolDefsPlan = filter(a.toolDefs), filter(a.toolDefsReadOnly), filter(a.toolDefsPlan)
}

// subagentAsking returns the running subagent waiting on toolCallID's
// permission or question answer, if any.
func (a *Agent) subagentAsking(toolCallID string) *Agent {
	a.mu.Lock()
	children := make([]*Agent, 0, len(a.subagents))
	for _, c := range a.subagents {
		children = append(children, c)
	}
	a.mu.Unlock()
	for _, c := range children {
		c.mu.Lock()
		_, perm := c.permRequests[toolCallID]
		_, ask := c.askUserRequests[toolCallID]
		c.mu.Unlock()
		if perm || ask {
			return c
		}
	}
	return nil
}
