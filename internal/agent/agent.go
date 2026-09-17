package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/diff"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"` // assistant only: the model's own reasoning/chain-of-thought text, if the provider exposed one
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`

	// AuthorName is who actually sent this message, for a shared session
	// with more than one connected peer — set only on the user message
	// that starts a turn (see RunTurn), left empty for assistant/tool
	// messages, which have no human author.
	AuthorName string `json:"author,omitempty"`

	// AuthorAvatar is that sender's picture, stored alongside the name so it
	// survives a reload: a client renders it from local settings for its own
	// live messages, but a transcript repainted from get_session_history has
	// nothing else to draw from. Either a short emoji glyph or a data URL
	// (SPEC.md §2a's conventions, devolved to whichever the client uses).
	AuthorAvatar string `json:"authorAvatar,omitempty"`
}

// Author identifies which peer's user_prompt started the current turn —
// threaded in via the context passed to RunTurn (see WithAuthor) rather
// than as a RunTurn parameter, so every existing/test caller that doesn't
// care about attribution keeps working unchanged with the zero value (no
// message attribution, no git-author env injection).
type Author struct {
	Name   string
	Email  string
	Avatar string
}

type authorContextKey struct{}

// WithAuthor attaches the peer identity that originated a turn to ctx,
// consumed by RunTurn for both chat attribution (Message.AuthorName) and
// git-author env injection (tool.ExecutionContext.AuthorName/AuthorEmail).
func WithAuthor(ctx context.Context, a Author) context.Context {
	return context.WithValue(ctx, authorContextKey{}, a)
}

// AuthorFromContext returns the zero Author if none was attached.
func AuthorFromContext(ctx context.Context) Author {
	a, _ := ctx.Value(authorContextKey{}).(Author)
	return a
}

// errSessionDiscarded aborts a save whose session was deleted while the turn
// was still finishing. Internal control flow, never surfaced to a client.
var errSessionDiscarded = errors.New("session was deleted")

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type StreamEventKind string

const (
	StreamChunkDelta    StreamEventKind = "delta"
	StreamChunkToolCall StreamEventKind = "tool_call"
	StreamChunkUsage    StreamEventKind = "usage"

	// StreamChunkReasoning carries a chunk of the model's own reasoning/
	// chain-of-thought text — Anthropic's extended-thinking `thinking_delta`
	// blocks, or an OpenAI-compatible provider's `reasoning_content` delta
	// (DeepSeek-reasoner and similar). Kept separate from StreamChunkDelta
	// so a client can render it as its own collapsible section instead of
	// mixing it into the visible answer.
	StreamChunkReasoning StreamEventKind = "reasoning"

	// StreamChunkFinish carries a provider's own stop/finish reason for the
	// round-trip that just ended. Providers only send it to flag a reason
	// other than a normal completion (e.g. output truncated at the token
	// limit) — its absence means "ended normally", so existing providers
	// that never emit it keep behaving exactly as before.
	StreamChunkFinish StreamEventKind = "finish"
)

type ModelStreamChunk struct {
	Kind         StreamEventKind
	Delta        string
	ToolCall     *ToolCall
	Usage        *protocol.UsageData
	FinishReason string
}

type ModelProvider interface {
	StreamTurn(ctx context.Context, history []Message, tools []tool.ToolDefinition) (<-chan ModelStreamChunk, <-chan error)
}

// TitleGenerator is implemented by providers that can make a separate,
// lightweight title request with the same model as the chat turn.
type TitleGenerator interface {
	GenerateTitle(ctx context.Context, prompt string) (string, error)
}

const maxChatTitleRunes = 60

// ModelOptions tunes a turn's request shape (see SetModelOptionsData on the
// wire). Fields are provider-specific; a zero value means "don't change".
type ModelOptions struct {
	ReasoningEffort string
	ThinkingBudget  int
}

// ModelOptionsSetter is implemented by providers whose request shape can be
// tuned per turn without swapping the provider itself (set_provider already
// covers a full swap). Providers that don't support any of it — CLI
// subprocesses, for instance — simply don't implement this interface.
type ModelOptionsSetter interface {
	SetModelOptions(ModelOptions)
}

// planModeReadOnlyTools is the allowlist of tools available while a session
// is in plan mode. Deliberately an allowlist, not a denylist, so a new
// mutating tool added later is excluded by default instead of accidentally
// exposed. See SPEC.md §4c: the model isn't given the write/bash tool
// definitions at all in plan mode, rather than being given them and denied
// at call time — it can't attempt a mutation there's nothing to intercept.
var planModeReadOnlyTools = map[string]bool{
	"file_read": true,
}

type Agent struct {
	stream           *protocol.IPCStream
	provider         ModelProvider
	tools            map[string]tool.Tool
	toolDefs         []tool.ToolDefinition
	toolDefsReadOnly []tool.ToolDefinition
	planMode         bool
	permEngine       *permission.Engine
	snapshotStore    snapshot.SnapshotStore
	sessionStore     SessionStore
	contextManager   *ContextManager
	usageStore       *UsageStore

	maxSessionSpendUSD float64 // 0 = unlimited

	sessionID         string
	history           []Message
	mu                sync.Mutex
	permRequests      map[string]chan permDecision
	permRequestKeys   map[string]string
	activeTurnCancels map[string]context.CancelFunc

	// discarded marks the session this Agent serves as deleted (see Discard):
	// a turn still in flight must not write back a file its caller just
	// removed. Read/written under mu.
	discarded bool

	// turnMu serializes RunTurn calls for this Agent (i.e. this session —
	// the registry hands out one Agent per sessionID, so this never blocks
	// a *different* session's turn, only two turns racing on the same one).
	// Without this, two user_prompts dispatched close together for the same
	// session could both read the pre-turn spend total before either wrote
	// it back (a TOCTOU on -max-session-cost-usd), and could interleave
	// a.history appends / sessionStore.Save calls non-deterministically.
	turnMu sync.Mutex
}

func NewAgent(
	stream *protocol.IPCStream,
	provider ModelProvider,
	tools []tool.Tool,
	engine *permission.Engine,
	snapStore snapshot.SnapshotStore,
	sessStore SessionStore,
	ctxMgr *ContextManager,
	usageStore *UsageStore,
) *Agent {
	toolMap := make(map[string]tool.Tool)
	var toolDefs []tool.ToolDefinition
	var toolDefsReadOnly []tool.ToolDefinition
	for _, t := range tools {
		toolMap[t.Name()] = t
		def := t.Definition()
		toolDefs = append(toolDefs, def)
		if planModeReadOnlyTools[t.Name()] {
			toolDefsReadOnly = append(toolDefsReadOnly, def)
		}
	}
	if ctxMgr == nil {
		ctxMgr = NewContextManager(64000, 4000)
	}

	return &Agent{
		stream:            stream,
		provider:          provider,
		tools:             toolMap,
		toolDefs:          toolDefs,
		toolDefsReadOnly:  toolDefsReadOnly,
		permEngine:        engine,
		snapshotStore:     snapStore,
		sessionStore:      sessStore,
		contextManager:    ctxMgr,
		usageStore:        usageStore,
		history:           make([]Message, 0),
		permRequests:      make(map[string]chan permDecision),
		permRequestKeys:   make(map[string]string),
		activeTurnCancels: make(map[string]context.CancelFunc),
	}
}

func (a *Agent) LoadSession(sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessionStore == nil {
		return nil
	}

	state, err := a.sessionStore.Load(sessionID)
	if err != nil {
		a.sessionID = sessionID
		a.history = make([]Message, 0)
		return nil
	}

	a.sessionID = state.SessionID
	a.history = state.History
	return nil
}

func (a *Agent) CurrentSessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// SetStream rebinds the agent to a new connection, e.g. when a client
// reconnects to an already-running session from a different device. The
// previously bound connection (if any and if different) is notified that it
// lost the session, since otherwise it would sit waiting for events that
// will now go to the new connection instead.
func (a *Agent) SetStream(stream *protocol.IPCStream) {
	a.mu.Lock()
	old := a.stream
	sessionID := a.sessionID
	a.stream = stream
	a.mu.Unlock()

	if old != nil && old != stream {
		_ = old.SendEvent(protocol.EventSessionTakenOver, sessionID, "", "", protocol.SessionTakenOverData{
			SessionID: sessionID,
		})
	}
}

func (a *Agent) getStream() *protocol.IPCStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stream
}

// SetProvider hot-swaps the model/provider a session uses. Takes effect on
// the next turn; an in-flight turn keeps using whichever provider it started
// with.
func (a *Agent) SetProvider(p ModelProvider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
}

func (a *Agent) getProvider() ModelProvider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

// SetModelOptions applies reasoning/thinking options to the current provider
// (if it supports them) and/or resizes the session's context-window budget.
// didApplyProviderOptions reports whether the current provider accepted the
// reasoning/thinking fields, so a caller can tell the client when they were
// silently ignored (e.g. a CLI-subprocess provider).
func (a *Agent) SetModelOptions(opts ModelOptions, contextWindow int) (didApplyProviderOptions bool) {
	a.mu.Lock()
	p := a.provider
	cm := a.contextManager
	a.mu.Unlock()

	if setter, ok := p.(ModelOptionsSetter); ok {
		setter.SetModelOptions(opts)
		didApplyProviderOptions = true
	}
	if contextWindow > 0 && cm != nil {
		cm.SetMaxHistoryTokens(contextWindow)
	}
	return didApplyProviderOptions
}

// SetPlanMode toggles a session between normal execution and plan mode.
// Takes effect on the next turn; an in-flight turn keeps whatever tool set
// it started with.
func (a *Agent) SetPlanMode(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planMode = enabled
}

func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planMode
}

// activeToolDefs returns the tool definitions to hand the model this turn —
// the read-only subset in plan mode, everything otherwise.
func (a *Agent) activeToolDefs() []tool.ToolDefinition {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.planMode {
		return a.toolDefsReadOnly
	}
	return a.toolDefs
}

// SetMaxSessionSpendUSD caps how much this session may have already spent
// (per the UsageStore) before a new turn is refused outright. 0 (the
// default) means unlimited. This is a per-turn-start gate, not a mid-turn
// kill switch: a single very expensive turn can still land above the cap,
// but a runaway sequence of turns won't keep compounding past it.
func (a *Agent) SetMaxSessionSpendUSD(limit float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maxSessionSpendUSD = limit
}

func (a *Agent) CancelTurn(turnID string) {
	a.mu.Lock()
	cancel, exists := a.activeTurnCancels[turnID]
	a.mu.Unlock()

	if exists && cancel != nil {
		cancel()
	}
}

// CancelAll cancels every turn currently running on this agent, e.g. before
// its session is deleted out from under it.
func (a *Agent) CancelAll() {
	a.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.activeTurnCancels))
	for _, cancel := range a.activeTurnCancels {
		cancels = append(cancels, cancel)
	}
	a.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// Discard marks the session this Agent serves as deleted. Removing the live
// handle and the persisted file isn't enough on its own: a turn that was in
// flight still saves when it finishes, which would write the file back. Every
// save this Agent makes from here on is dropped instead. Call this before the
// file is removed, and before CancelAll, so an aborting turn can't win the
// race.
func (a *Agent) Discard() {
	a.mu.Lock()
	a.discarded = true
	a.mu.Unlock()
}

func (a *Agent) isDiscarded() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.discarded
}

// permDecision carries a permission_response back to the turn loop waiting
// on it: the allow/deny verdict plus an optional denial reason the user
// typed, so a rejected tool call's result can tell the model why instead of
// the fixed "permission denied by user or policy" string.
type permDecision struct {
	allowed  bool
	feedback string
}

func (a *Agent) HandlePermissionResponse(toolCallID string, allowed bool, remember bool, feedback string) {
	a.mu.Lock()
	ch, exists := a.permRequests[toolCallID]
	cacheKey := a.permRequestKeys[toolCallID]
	delete(a.permRequests, toolCallID)
	delete(a.permRequestKeys, toolCallID)
	a.mu.Unlock()

	if remember && cacheKey != "" {
		a.permEngine.RecordDecision(cacheKey, allowed, true)
	}

	if exists {
		ch <- permDecision{allowed: allowed, feedback: feedback}
	}
}

func (a *Agent) RunTurn(parentCtx context.Context, sessionID, turnID, prompt string) error {
	return a.RunTurnWithTitleGeneration(parentCtx, sessionID, turnID, prompt, true)
}

// RunTurnWithTitleGeneration runs a turn and titles a newly-created chat in
// parallel. Existing callers use RunTurn, whose default is enabled.
func (a *Agent) RunTurnWithTitleGeneration(parentCtx context.Context, sessionID, turnID, prompt string, autoGenerateTitle bool) error {
	// Serialize turns within this session: see turnMu's doc comment. This
	// must wrap the budget check through the final usageStore.Record write
	// (both happen below) so the check-then-record isn't itself racy.
	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	startTime := time.Now()
	author := AuthorFromContext(parentCtx)

	a.mu.Lock()
	if a.sessionID == "" || a.sessionID != sessionID {
		a.sessionID = sessionID
	}
	a.mu.Unlock()

	turnProvider := a.getProvider()
	if turnProvider == nil {
		_ = a.getStream().SendEvent(protocol.EventTurnCompleted, sessionID, turnID, "", a.buildTurnCompletedData("no_provider", protocol.UsageData{}, startTime))
		return fmt.Errorf("session %s has no provider configured — call register_provider and set_provider first", sessionID)
	}

	a.mu.Lock()
	spendCap := a.maxSessionSpendUSD
	a.mu.Unlock()
	if a.usageStore != nil && spendCap > 0 {
		if spent := a.usageStore.SessionTotal(sessionID).CostUSD; spent >= spendCap {
			_ = a.getStream().SendEvent(protocol.EventTurnCompleted, sessionID, turnID, "", a.buildTurnCompletedData("budget_exceeded", protocol.UsageData{}, startTime))
			return fmt.Errorf("session %s has already spent $%.4f, at or above its $%.4f cap", sessionID, spent, spendCap)
		}
	}

	turnCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	a.mu.Lock()
	isNewChat := len(a.history) == 0
	a.history = append(a.history, Message{
		Role:         "user",
		Content:      prompt,
		AuthorName:   author.Name,
		AuthorAvatar: author.Avatar,
	})
	a.activeTurnCancels[turnID] = cancel
	a.mu.Unlock()

	// applyGeneratedTitle saves `title` (if still empty) and broadcasts the
	// resulting session_updated — the only place an auto-generated title ever
	// reaches a connected client, since it has no client-driven
	// set_session_title to piggyback on.
	applyGeneratedTitle := func(sessID, title string) {
		if title == "" || a.sessionStore == nil {
			return
		}
		titleJustSet := false
		state, err := a.sessionStore.Update(sessID, func(state *SessionState) error {
			if a.isDiscarded() {
				return errSessionDiscarded
			}
			if state.Title == "" {
				state.Title = title
				titleJustSet = true
			}
			return nil
		})
		if err == nil && titleJustSet {
			_ = a.getStream().SendEvent(protocol.EventSessionUpdated, sessID, turnID, "", protocol.SessionSummary{
				SessionID:       state.SessionID,
				UpdatedAt:       state.UpdatedAt,
				MessageCount:    len(state.History),
				Title:           state.Title,
				Archived:        state.Archived,
				LinkedSessionID: state.LinkedSessionID,
			})
		}
	}

	if isNewChat {
		// Runs — and saves/broadcasts — independently of the turn it started
		// alongside: a title request is a small, non-streaming call that
		// resolves in well under a second, so gating it behind the full turn
		// (which may run many tool calls before it's done) would make title
		// generation look like it only starts once the answer is already in,
		// instead of racing it the way SPEC.md §3.1 describes.
		go func(sessID string) {
			title := fallbackTitle(prompt)
			if autoGenerateTitle {
				if generator, ok := turnProvider.(TitleGenerator); ok {
					if generated, err := generator.GenerateTitle(turnCtx, prompt); err == nil {
						title = normalizeTitle(generated)
					}
				}
			}
			applyGeneratedTitle(sessID, title)
		}(sessionID)
	}

	// persistTurn writes this turn's history to the session store, leaving
	// metadata changed through set_session_* while this Agent was live intact
	// (see SessionStore.Update for why that read-modify-write has to be
	// atomic). Never holds a.mu across the write — CancelTurn takes that mutex,
	// and a save must not be able to delay an abort.
	persistTurn := func() error {
		a.mu.Lock()
		sessID := a.sessionID
		history := append([]Message(nil), a.history...)
		a.mu.Unlock()
		if a.sessionStore == nil || sessID == "" {
			return nil
		}
		_, err := a.sessionStore.Update(sessID, func(state *SessionState) error {
			// Re-checked here, under the store's own write lock — the same lock
			// SessionStore.Delete takes. Either this save lands before the delete
			// (and the file is removed right after it) or the delete landed first,
			// in which case writing now would resurrect a session the user just
			// deleted.
			if a.isDiscarded() {
				return errSessionDiscarded
			}
			state.History = history
			return nil
		})
		if errors.Is(err, errSessionDiscarded) {
			return nil
		}
		return err
	}

	defer func() {
		a.mu.Lock()
		delete(a.activeTurnCancels, turnID)
		a.mu.Unlock()
		if a.snapshotStore != nil {
			a.snapshotStore.CloseTurn(turnID)
		}
	}()

	// Write the user's own message to disk before the model is asked anything at
	// all. Every way out of this function below persists again (the final save),
	// so this first write is what makes the prompt itself durable against a
	// cancel, a provider that never responds, or the process being killed before
	// the turn ends — and it's what makes the session file exist on a first
	// message, which set_session_title requires (SPEC.md §3.1) and which used to
	// only appear once a turn had already completed successfully.
	if err := persistTurn(); err != nil {
		// Reported without a turnId on purpose: an error carrying this turn's id
		// would make the client tear down its streaming state while the model is
		// still answering. The save below reports it properly if it's still
		// failing by then.
		_ = a.getStream().SendEvent(protocol.EventError, sessionID, "", "", protocol.ErrorData{
			Message: fmt.Sprintf("session %s could not be saved yet (%v) — this turn may be lost if the server stops", sessionID, err),
		})
	}

	var (
		totalUsage    protocol.UsageData
		finishReason  string
		streamErr     error
		cutShort      bool
		assistantText string
		toolCalls     []ToolCall
	)

	// emptyLengthRetries caps how many times a round-trip that produced no
	// visible text and no tool call — just finish_reason "length" — gets
	// silently retried before giving up and reporting the cutoff. This is
	// the reasoning-model failure mode: the whole token budget goes to
	// hidden reasoning before any answer text is written, so what streamed
	// out is nothing at all rather than a partial answer. A retry is a
	// fresh, independently-sampled generation, not a resend of the same
	// doomed request, so it can (and typically does) land differently.
	const maxEmptyLengthRetries = 2
	var emptyLengthRetries int

	// One model round-trip per iteration; each iteration either finishes the turn
	// (no tool calls), runs tools and loops, or breaks out early when the turn is
	// cancelled or the provider fails mid-stream.
turn:
	for {
		assistantText, toolCalls = "", nil
		var reasoningText string
		// ponytail: only surfaced when this round-trip ends with no tool
		// calls (below) — a round truncated mid tool-call JSON still executes
		// that (possibly malformed) call instead of being flagged, since
		// telling those two cases apart needs buffering tool calls until the
		// stream's final stop reason is known. Tool execution's own
		// json.Unmarshal error is the fallback signal for that case today.
		var providerFinishReason string
		select {
		case <-turnCtx.Done():
			finishReason, cutShort = "cancelled", true
			break turn
		default:
		}

		a.mu.Lock()
		activeContext := a.contextManager.PrepareHistory(a.history)
		a.mu.Unlock()

		chunks, errs := turnProvider.StreamTurn(turnCtx, activeContext, a.activeToolDefs())

		done := false
		for !done {
			select {
			case <-turnCtx.Done():
				finishReason, cutShort = "cancelled", true
				done = true
			case err, ok := <-errs:
				if ok && err != nil {
					if errors.Is(err, context.Canceled) {
						finishReason, cutShort = "cancelled", true
					} else {
						streamErr = fmt.Errorf("model stream error: %w", err)
					}
					done = true
				}
			case chunk, ok := <-chunks:
				if !ok {
					// The chunk channel closing is how a stream ends, so establish
					// *why* before calling this a finished turn. A provider tears its
					// channels down both when it's done and when it's aborted or has
					// failed (the error buffered in eChan, then both channels closed),
					// which can leave this select with several ready cases at once —
					// and it picks between them at random. Without this, a truncated
					// stream gets reported as a completed turn, and a turn the user
					// stopped gets reported as one that finished on its own.
					switch {
					case turnCtx.Err() != nil:
						finishReason, cutShort = "cancelled", true
					default:
						select {
						case err, errOK := <-errs:
							if errOK && err != nil {
								if errors.Is(err, context.Canceled) {
									finishReason, cutShort = "cancelled", true
								} else {
									streamErr = fmt.Errorf("model stream error: %w", err)
								}
							}
						default:
						}
					}
					done = true
					break
				}
				switch chunk.Kind {
				case StreamChunkDelta:
					assistantText += chunk.Delta
					_ = a.getStream().SendEvent(protocol.EventModelDelta, a.sessionID, turnID, "", protocol.ModelDeltaData{
						Content: chunk.Delta,
					})
				case StreamChunkReasoning:
					reasoningText += chunk.Delta
					_ = a.getStream().SendEvent(protocol.EventModelDelta, a.sessionID, turnID, "", protocol.ModelDeltaData{
						Reasoning: chunk.Delta,
					})
				case StreamChunkToolCall:
					if chunk.ToolCall != nil {
						toolCalls = append(toolCalls, *chunk.ToolCall)
						_ = a.getStream().SendEvent(protocol.EventToolCallRequested, a.sessionID, turnID, chunk.ToolCall.ID, protocol.ToolCallData{
							Name: chunk.ToolCall.Name,
							Args: chunk.ToolCall.Args,
						})
					}
				case StreamChunkUsage:
					if chunk.Usage != nil {
						totalUsage.InputTokens += chunk.Usage.InputTokens
						totalUsage.OutputTokens += chunk.Usage.OutputTokens
						totalUsage.TotalTokens += chunk.Usage.TotalTokens
						if chunk.Usage.CostKnown {
							totalUsage.CostUSD += chunk.Usage.CostUSD
							totalUsage.CostKnown = true
						}
					}
				case StreamChunkFinish:
					providerFinishReason = chunk.FinishReason
				}
			}
		}

		if cutShort || streamErr != nil {
			break turn
		}

		// A round-trip that produced neither text nor a tool call, cut off
		// by the token limit rather than finishing on its own: nothing was
		// generated at all (not even a partial answer), so unlike an
		// ordinary "length" cutoff there's nothing worth keeping — retry
		// instead of reporting a blank turn. History wasn't touched (the
		// guard below never runs for an empty round), so this is a fresh
		// independently-sampled generation, not a resend of the same
		// doomed request.
		if assistantText == "" && len(toolCalls) == 0 && providerFinishReason == "length" && emptyLengthRetries < maxEmptyLengthRetries {
			emptyLengthRetries++
			continue turn
		}

		// A round-trip that produced neither text nor a tool call (a
		// provider can finish_reason="length" with nothing generated —
		// e.g. a reasoning model that spends its whole token budget on
		// hidden reasoning before writing any visible content) must not
		// become an assistant message with no content and no tool_calls:
		// every provider's wire format requires one or the other, so that
		// message would make every future request in this session invalid
		// — the next turn fails outright instead of just this one.
		if assistantText != "" || len(toolCalls) > 0 {
			a.mu.Lock()
			a.history = append(a.history, Message{
				Role:      "assistant",
				Content:   assistantText,
				Reasoning: reasoningText,
				ToolCalls: toolCalls,
			})
			a.mu.Unlock()
		}

		if len(toolCalls) == 0 {
			// A provider-reported reason other than a normal completion (e.g.
			// output truncated at the token limit) overrides the default —
			// see StreamChunkFinish's doc comment.
			if providerFinishReason != "" {
				finishReason = providerFinishReason
			} else {
				finishReason = "stop"
			}
			break
		}

		for _, tc := range toolCalls {
			toolImpl, exists := a.tools[tc.Name]
			if !exists {
				a.recordToolResult(turnID, tc, "", fmt.Sprintf("tool %s not recognized", tc.Name))
				continue
			}
			if a.PlanMode() && !planModeReadOnlyTools[tc.Name] {
				a.recordToolResult(turnID, tc, "", fmt.Sprintf("tool %s is unavailable while in plan mode", tc.Name))
				continue
			}

			allowed := true
			var denyFeedback string
			if toolImpl.RequiresPermission(tc.Args) {
				var target string
				if tc.Name == "bash" {
					var bArgs struct{ Command string }
					_ = json.Unmarshal(tc.Args, &bArgs)
					target = bArgs.Command
				} else {
					var fArgs struct{ Path string }
					_ = json.Unmarshal(tc.Args, &fArgs)
					target = fArgs.Path
				}

				decision, cacheKey := a.permEngine.Evaluate(tc.Name, target)

				switch decision {
				case permission.DecisionDeny:
					allowed = false
				case permission.DecisionAsk:
					respCh := make(chan permDecision, 1)
					a.mu.Lock()
					a.permRequests[tc.ID] = respCh
					a.permRequestKeys[tc.ID] = cacheKey
					a.mu.Unlock()

					// Compute unified diff preview if tool is diffable
					var computedDiff *diff.FileDiff
					if diffTool, isDiffable := toolImpl.(tool.DiffableTool); isDiffable {
						if d, dErr := diffTool.PreviewDiff(tc.Args); dErr == nil {
							computedDiff = d
						}
					}

					_ = a.getStream().SendEvent(protocol.EventPermissionRequest, a.sessionID, turnID, tc.ID, protocol.PermissionRequestData{
						ToolName:    tc.Name,
						Description: fmt.Sprintf("%s on %s", tc.Name, string(tc.Args)),
						RequiresAsk: true,
						CacheKey:    cacheKey,
						Diff:        computedDiff,
					})

					select {
					case <-turnCtx.Done():
						finishReason, cutShort = "cancelled", true
						break turn
					case decision := <-respCh:
						allowed = decision.allowed
						denyFeedback = decision.feedback
					}
				case permission.DecisionAllow:
					allowed = true
				}
			}

			if !allowed {
				reason := "permission denied by user or policy"
				if denyFeedback != "" {
					reason = fmt.Sprintf("permission denied by user: %s", denyFeedback)
				}
				a.recordToolResult(turnID, tc, "", reason)
				continue
			}

			execCtx := tool.ExecutionContext{
				SessionID:     a.sessionID,
				TurnID:        turnID,
				ToolCallID:    tc.ID,
				SnapshotStore: a.snapshotStore,
				Stream:        a.getStream(),
				AuthorName:    author.Name,
				AuthorEmail:   author.Email,
			}

			output, execErr := toolImpl.Execute(turnCtx, execCtx, tc.Args)
			truncatedOutput := a.contextManager.TruncateToolOutput(output)

			if execErr != nil {
				a.recordToolResult(turnID, tc, "", execErr.Error())
			} else {
				a.recordToolResult(turnID, tc, truncatedOutput, "")
			}
		}
	}

	// Whatever text had already streamed before the turn was cut short is part
	// of what the user watched arrive, so it's kept rather than leaving the
	// prompt as the turn's only record. (Only for the cut-short exits: a
	// completed round-trip already appended its assistant message above.) A
	// cut-short turn that produced nothing adds no empty assistant message.
	if (cutShort || streamErr != nil) && (assistantText != "" || len(toolCalls) > 0) {
		a.mu.Lock()
		a.history = append(a.history, Message{
			Role:      "assistant",
			Content:   assistantText,
			ToolCalls: toolCalls,
		})
		a.mu.Unlock()
	}

	// The one save every way out of this function goes through — normal
	// completion, a cancel, a provider failure. Not discarded: the turn's
	// content exists nowhere else, so a failed write means the conversation the
	// user just had is not on disk, and saying nothing would report a turn that
	// can't be reloaded as an ordinary success.
	if saveErr := persistTurn(); saveErr != nil {
		_ = a.getStream().SendEvent(protocol.EventError, a.sessionID, turnID, "", protocol.ErrorData{
			Message: fmt.Sprintf("session %s could not be saved (%v) — this turn will not survive a restart", a.sessionID, saveErr),
		})
	}

	// A provider failure has no finishReason to report — the dispatcher emits
	// the `error` frame for it (§3.2 has no "error" finish reason), so
	// turn_completed is reserved for the reasons it does define.
	if finishReason != "" {
		_ = a.getStream().SendEvent(protocol.EventTurnCompleted, a.sessionID, turnID, "", a.buildTurnCompletedData(finishReason, totalUsage, startTime))
	}

	if streamErr != nil {
		return streamErr
	}
	if cutShort {
		return turnCtx.Err()
	}
	return nil
}

func fallbackTitle(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) > 8 {
		words = words[:8]
	}
	return limitTitle(strings.Join(words, " "))
}

func normalizeTitle(title string) string {
	return limitTitle(strings.Trim(strings.Join(strings.Fields(title), " "), "\"'“”‘’"))
}

func limitTitle(title string) string {
	runes := []rune(title)
	if len(runes) > maxChatTitleRunes {
		return string(runes[:maxChatTitleRunes])
	}
	return string(runes)
}

// buildTurnCompletedData assembles a turn_completed payload including the
// context-window fullness and running spend, so every emission point (normal
// completion and each cancellation path) reports the same enriched data.
func (a *Agent) buildTurnCompletedData(finishReason string, usage protocol.UsageData, startTime time.Time) protocol.TurnCompletedData {
	a.mu.Lock()
	sessionID := a.sessionID
	historySnapshot := a.history
	a.mu.Unlock()

	// A provider (e.g. a CLI subprocess) may already report an authoritative
	// cost; only fall back to the pricing table when it hasn't (CostKnown
	// distinguishes "no cost reported" from "provider reported a real $0").
	if !usage.CostKnown {
		if modelNamer, ok := a.getProvider().(interface{ ModelName() string }); ok {
			usage.CostUSD = EstimateCostUSD(modelNamer.ModelName(), usage)
		}
	}

	var spend *protocol.SpendData
	if a.usageStore != nil {
		sessTotals, projTotals := a.usageStore.Record(sessionID, usage.InputTokens, usage.OutputTokens, usage.CostUSD)
		spend = &protocol.SpendData{SessionTotalUSD: sessTotals.CostUSD, ProjectTotalUSD: projTotals.CostUSD}
	}

	// The provider's own reported token counts (its actual tokenizer) beat
	// our char/4 heuristic whenever we have them: the heuristic also never
	// accounts for the system prompt or tool schemas sent with every
	// request, so it reads as "less full" than the context window actually
	// is. InputTokens+OutputTokens is what this turn just added to the
	// history that gets resent next turn, so it's the right number for
	// "how full is the window now" — not just this round-trip's input.
	estimatedTokens := a.contextManager.EstimateHistoryTokens(historySnapshot)
	if usage.InputTokens > 0 {
		estimatedTokens = usage.InputTokens + usage.OutputTokens
	}

	return protocol.TurnCompletedData{
		FinishReason: finishReason,
		SessionID:    sessionID,
		Usage:        &usage,
		Context: &protocol.ContextData{
			EstimatedTokens: estimatedTokens,
			MaxTokens:       a.contextManager.GetMaxHistoryTokens(),
		},
		Spend:      spend,
		DurationMs: time.Since(startTime).Milliseconds(),
	}
}

func (a *Agent) recordToolResult(turnID string, tc ToolCall, output, errText string) {
	a.mu.Lock()
	a.history = append(a.history, Message{
		Role:       "tool",
		Content:    output + errText,
		ToolCallID: tc.ID,
	})
	sessID := a.sessionID
	a.mu.Unlock()

	_ = a.getStream().SendEvent(protocol.EventToolResult, sessID, turnID, tc.ID, protocol.ToolResultData{
		ToolName: tc.Name,
		Output:   output,
		Error:    errText,
	})
}
