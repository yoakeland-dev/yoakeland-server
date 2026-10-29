package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"yoakeland/internal/protocol"
)

const sideSystemPrompt = `You answer a side question about a coding session. The conversation so far is in the user's message as a transcript. You have no tools and cannot change anything; answer from the transcript alone.
Be brief and direct. If the transcript doesn't say, say so rather than guess.`

const summarySystemPrompt = `You are the coding agent that wrote the reply in the user's message. Recap it for the user in the first person ("I fixed…", "I found…", "I couldn't…"), and suggest what they might do next. Answer with only a JSON object:
{"summary": "...", "ask": "...", "actions": [{"label": "...", "prompt": "..."}]}
summary: two or three short sentences in the first person, at most 60 words: what I did or found, and anything the user must act on. Plain text.
ask: one short question in the first person that offers the most useful next step ("Want me to run the tests?"). Empty when there are no actions.
actions: zero to four likely next steps that follow from the reply (verify it, fix something it found, go deeper, undo). label: two to four words, the text on a button. prompt: the full, specific instruction to send the agent for that step, written as the user, thorough enough to run on its own. Only steps the reply makes sensible; none if it was just an answer.`

const maxSummaryInput = 24000

// sideBinding is the provider a side request runs on: the utility role when
// one is assigned, else the session's own. A native agent has no plain model
// to ask.
func (a *Agent) sideBinding() (RoleBinding, error) {
	b := a.roleOr(RoleUtility, a.turnBinding())
	if b.Provider == nil {
		return b, errors.New("no provider configured for this session")
	}
	if _, native := b.Provider.(NativeAgent); native {
		if _, ok := b.Provider.(OneShotter); !ok {
			return b, errors.New("this agent can't answer side questions; assign a utility model in Settings")
		}
	}
	return b, nil
}

// oneShot streams one tool-less request and returns what it said. onDelta may be nil.
func (a *Agent) oneShot(ctx context.Context, b RoleBinding, prompt, system string, onDelta func(string)) (string, protocol.UsageData, error) {
	var usage protocol.UsageData
	var err error
	var text string
	if os, ok := b.Provider.(OneShotter); ok && isNative(b.Provider) {
		text, usage, err = os.OneShot(ctx, system, prompt)
		if text != "" && onDelta != nil {
			onDelta(text)
		}
	} else {
		chunks, errs := b.Provider.StreamTurn(ctx, []Message{{Role: "user", Content: prompt}}, nil, system)
		var sb strings.Builder
		for c := range chunks {
			switch c.Kind {
			case StreamChunkDelta:
				sb.WriteString(c.Delta)
				if onDelta != nil {
					onDelta(c.Delta)
				}
			case StreamChunkUsage:
				if c.Usage != nil {
					usage = *c.Usage
				}
			}
		}
		err = <-errs
		text = sb.String()
		priceUsage(b.Model, &usage)
	}
	if a.usageStore != nil && a.sessionID != "" {
		if usage.CostUnknown {
			a.usageStore.MarkCostUnknown(a.CurrentSessionID())
		}
		a.usageStore.Record(a.CurrentSessionID(), usage.InputTokens, usage.OutputTokens, usage.CostUSD)
	}
	return strings.TrimSpace(text), usage, err
}

func isNative(p ModelProvider) bool {
	_, ok := p.(NativeAgent)
	return ok
}

// SideQuestion answers q about the session without taking its turn slot, so it
// works while a turn runs, and without adding to the conversation. With an
// anchor the exchange is kept on that message as a thread. emit gets each
// piece of the answer; the last has Done set.
func (a *Agent) SideQuestion(ctx context.Context, sessionID, requestID, anchorID, q string, emit func(protocol.SideAnswerData)) {
	fail := func(err error) {
		emit(protocol.SideAnswerData{RequestID: requestID, AnchorMessageID: anchorID, Done: true, Error: err.Error()})
	}
	q = strings.TrimSpace(q)
	if q == "" {
		fail(errors.New("empty question"))
		return
	}
	b, err := a.sideBinding()
	if err != nil {
		fail(err)
		return
	}

	a.mu.Lock()
	history := append([]Message(nil), a.history...)
	a.mu.Unlock()

	var thread []protocol.ThreadMessage
	if anchorID != "" {
		i, found := indexOfMessage(history, anchorID)
		if !found {
			fail(errors.New("that message is gone"))
			return
		}
		thread = history[i].Thread
		history = history[:i+1] // a thread talks about its message, not what came after
	}

	var p strings.Builder
	p.WriteString("<transcript>\n")
	p.WriteString(transcriptForSummary(contextView(history, "")))
	p.WriteString("</transcript>\n")
	if len(thread) > 0 {
		p.WriteString("\nThis side thread so far:\n")
		for _, m := range thread {
			fmt.Fprintf(&p, "%s: %s\n", strings.ToUpper(m.Role[:1])+m.Role[1:], m.Content)
		}
	}
	p.WriteString("\nSide question: " + q)

	text, _, err := a.oneShot(ctx, b, p.String(), sideSystemPrompt, func(d string) {
		emit(protocol.SideAnswerData{RequestID: requestID, AnchorMessageID: anchorID, Delta: d})
	})
	if err != nil {
		fail(err)
		return
	}
	if anchorID != "" {
		now := time.Now()
		a.mu.Lock()
		// The history may have been edited under us; keep the exchange only if the anchor survived.
		if i, ok := indexOfMessage(a.history, anchorID); ok {
			a.history[i].Thread = append(append([]protocol.ThreadMessage(nil), a.history[i].Thread...),
				protocol.ThreadMessage{Role: "user", Content: q, CreatedAt: now},
				protocol.ThreadMessage{Role: "assistant", Content: text, CreatedAt: time.Now()})
		}
		a.mu.Unlock()
		_ = a.persistCurrentState()
	}
	emit(protocol.SideAnswerData{RequestID: requestID, AnchorMessageID: anchorID, Done: true, Text: text})
}

// SummarizeMessage writes a short summary of one assistant message onto it, with the
// follow-ups it suggests.
func (a *Agent) SummarizeMessage(ctx context.Context, messageID string) (string, string, []protocol.QuickAction, error) {
	b, err := a.sideBinding()
	if err != nil {
		return "", "", nil, err
	}
	a.mu.Lock()
	var content string
	if i, ok := indexOfMessage(a.history, messageID); ok && a.history[i].Role == "assistant" {
		content = a.history[i].Content
	}
	a.mu.Unlock()
	if strings.TrimSpace(content) == "" {
		return "", "", nil, errors.New("nothing to summarize")
	}
	raw, _, err := a.oneShot(ctx, b, truncateRunes(content, maxSummaryInput), summarySystemPrompt, nil)
	if err != nil {
		return "", "", nil, err
	}
	summary, ask, actions := parseSummary(raw)
	if summary == "" {
		return "", "", nil, errors.New("the model returned no summary")
	}
	a.mu.Lock()
	if i, ok := indexOfMessage(a.history, messageID); ok {
		a.history[i].Summary, a.history[i].Ask, a.history[i].Actions = summary, ask, actions
	}
	a.mu.Unlock()
	_ = a.persistCurrentState()
	return summary, ask, actions, nil
}

// parseSummary reads the model's JSON answer, tolerating a code fence around it. A model
// that ignored the format and just wrote a summary still gets one, with no actions.
func parseSummary(raw string) (string, string, []protocol.QuickAction) {
	raw = strings.TrimSpace(raw)
	body := raw
	if i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); i >= 0 && j > i {
		body = raw[i : j+1]
	}
	var out struct {
		Summary string                 `json:"summary"`
		Ask     string                 `json:"ask"`
		Actions []protocol.QuickAction `json:"actions"`
	}
	if json.Unmarshal([]byte(body), &out) != nil || strings.TrimSpace(out.Summary) == "" {
		return raw, "", nil
	}
	var actions []protocol.QuickAction
	for _, a := range out.Actions {
		a.Label, a.Prompt = strings.TrimSpace(a.Label), strings.TrimSpace(a.Prompt)
		if a.Label != "" && a.Prompt != "" && len(actions) < 4 {
			actions = append(actions, a)
		}
	}
	ask := strings.TrimSpace(out.Ask)
	if len(actions) == 0 {
		ask = ""
	}
	return strings.TrimSpace(out.Summary), ask, actions
}
