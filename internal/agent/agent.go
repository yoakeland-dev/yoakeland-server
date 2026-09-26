package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
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
	// ID addresses this message for edit_message/retry_message/switch_branch
	// — minted once, at append time, from the session's own counter
	// (nextMessageIDLocked). Empty only for a message persisted before this
	// field existed; LoadSession migrates those in place on first load.
	ID         string     `json:"id,omitempty"`
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"` // assistant only: the model's own reasoning/chain-of-thought text, if the provider exposed one
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	// Diffs: file changes a native tool call reported (tool messages only).
	Diffs []*diff.FileDiff `json:"diffs,omitempty"`

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

	// Attachments are server-side paths this user message attached (see
	// WithAttachments). They stay readable by file_read for the rest of the
	// session, even outside the project root, and the model is told about
	// them via attachmentNote rather than by rewriting Content.
	Attachments []string `json:"attachments,omitempty"`

	// Provider/Model are the registered provider name and model id that
	// generated this message (assistant only — empty for user/tool
	// messages). Stamped once per round-trip so a session's model footer
	// stays correct in get_session_history even after the session's own
	// provider/model selection has since changed.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// CreatedAt is when this message was produced. GenerationMs is how long
	// the model round-trip that produced it took (assistant only — the
	// provider request/stream itself, not any tool execution before or
	// after it in the same turn).
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	GenerationMs int64     `json:"generationMs,omitempty"`

	// Usage is this round-trip's own token/cost accounting (assistant only)
	// — not the whole turn's. A provider's InputTokens is already the full
	// conversation resent as this request's input, so it's naturally "tokens
	// in up to this point" with no separate running total to track;
	// OutputTokens is naturally just this round-trip's own reply.
	Usage *protocol.UsageData `json:"usage,omitempty"`
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

type attachmentsContextKey struct{}

// WithAttachments attaches a user_prompt's attachment paths to ctx, consumed
// by RunTurn the same way WithAuthor is — so no RunTurn signature change.
func WithAttachments(ctx context.Context, paths []string) context.Context {
	return context.WithValue(ctx, attachmentsContextKey{}, paths)
}

func attachmentsFromContext(ctx context.Context) []string {
	p, _ := ctx.Value(attachmentsContextKey{}).([]string)
	return p
}

// withAttachmentNotes returns history with each attaching user message's
// content suffixed by a note naming its files, for the provider only — the
// stored Content (and so the transcript) stays exactly what the user typed.
func withAttachmentNotes(history []Message) []Message {
	var out []Message
	for i, m := range history {
		if len(m.Attachments) == 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), history...)
		}
		out[i].Content = m.Content + "\n\n[Attached files — read them with file_read using these exact absolute paths: " + strings.Join(m.Attachments, ", ") + "]"
	}
	if out == nil {
		return history
	}
	return out
}

// sessionAttachmentsLocked is every path attached anywhere in the active
// history — file_read's extra allowlist (tool.ExecutionContext.ExtraReadPaths).
func (a *Agent) sessionAttachmentsLocked() []string {
	var paths []string
	for _, m := range a.history {
		paths = append(paths, m.Attachments...)
	}
	return paths
}

// AuthorFromContext returns the zero Author if none was attached.
func AuthorFromContext(ctx context.Context) Author {
	a, _ := ctx.Value(authorContextKey{}).(Author)
	return a
}

// errSessionDiscarded aborts a save whose session was deleted while the turn
// was still finishing. Internal control flow, never surfaced to a client.
var errSessionDiscarded = errors.New("session was deleted")

// Sentinel errors for EditMessage/RetryMessage/SwitchBranch, matching
// ErrSessionNotFound's pattern so a dispatcher can report a stable reason
// without string-matching.
var (
	ErrMessageNotFound     = errors.New("message not found")
	ErrMessageNotEditable  = errors.New("only a user message can be edited")
	ErrMessageNotRetryable = errors.New("only an assistant message can be retried")
)

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	// Set only for a call a NativeAgent ran itself (see native.go) — never
	// executed by this loop, and flattened to text for any other provider.
	protocol.NativeToolInfo
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

	// StreamChunkToolResult is a NativeAgent reporting the outcome of a tool
	// call it already ran itself (see native.go).
	StreamChunkToolResult StreamEventKind = "tool_result"
)

type ModelStreamChunk struct {
	Kind         StreamEventKind
	Delta        string
	ToolCall     *ToolCall
	ToolResult   *NativeToolResult // StreamChunkToolResult only
	Usage        *protocol.UsageData
	FinishReason string
}

type ModelProvider interface {
	StreamTurn(ctx context.Context, history []Message, tools []tool.ToolDefinition, systemPrompt string) (<-chan ModelStreamChunk, <-chan error)
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

// Mode selects which tools the model is even given (SPEC.md §4c) and, for
// bash specifically, whether the permission engine's normal decision is
// overridden. Four values, no bool — see SPEC.md §4c for the tier
// descriptions.
type Mode string

const (
	ModeReadOnly   Mode = "read_only"
	ModeReadWrite  Mode = "read_write"
	ModeAutonomous Mode = "autonomous"
	ModePlan       Mode = "plan"
)

// readOnlyTools is the allowlist of tools available in ModeReadOnly and
// ModePlan. Deliberately an allowlist, not a denylist, so a new mutating
// tool added later is excluded by default instead of accidentally exposed.
// See SPEC.md §4c: the model isn't given the write/bash tool definitions at
// all, rather than being given them and denied at call time — it can't
// attempt a mutation there's nothing to intercept.
var readOnlyTools = map[string]bool{
	"file_read":         true,
	"ask_user_question": true,
}

// planExtraTools is unioned into readOnlyTools for ModePlan only — its own
// designated output channel, not available in ModeReadOnly.
var planExtraTools = map[string]bool{
	"write_plan": true,
}

type Agent struct {
	stream           *protocol.StreamSet
	provider         ModelProvider
	providerName     string // registered name of `provider` (SPEC.md §3.1 set_provider) — stamped onto each assistant Message, not just kept for internal use
	tools            map[string]tool.Tool
	toolDefs         []tool.ToolDefinition
	toolDefsReadOnly []tool.ToolDefinition
	toolDefsPlan     []tool.ToolDefinition
	mode             Mode
	webToolsEnabled  bool
	permEngine       *permission.Engine
	snapshotStore    snapshot.SnapshotStore
	sessionStore     SessionStore
	contextManager   *ContextManager
	usageStore       *UsageStore

	maxSessionSpendUSD float64 // 0 = unlimited

	sessionID         string
	history           []Message
	branches          map[string][][]Message // see SessionState.Branches
	nextMsgSeq        int                    // see SessionState.NextMsgSeq
	mu                sync.Mutex
	permRequests      map[string]chan permDecision
	permRequestKeys   map[string]string
	askUserRequests   map[string]chan string
	activeTurnCancels map[string]context.CancelFunc

	// discarded marks the session this Agent serves as deleted (see Discard):
	// a turn still in flight must not write back a file its caller just
	// removed. Read/written under mu.
	discarded bool

	// turnSlot (capacity 1) serializes RunTurn calls for this Agent (i.e.
	// this session — the registry hands out one Agent per sessionID, so this
	// never blocks a *different* session's turn, only two turns racing on the
	// same one). Without this, two user_prompts dispatched close together for
	// the same session could both read the pre-turn spend total before either
	// wrote it back (a TOCTOU on -max-session-cost-usd), and could interleave
	// a.history appends / sessionStore.Save calls non-deterministically. A
	// channel rather than a mutex so a queued turn can stop waiting (lockTurn).
	turnSlot chan struct{}

	// titleWG tracks runTitleGeneration goroutines, which outlive the turn
	// that started them by design; tests wait on it before their temp
	// project dir is removed (export_test.go).
	titleWG sync.WaitGroup

	// queuedTurns holds the cancel of every turn waiting for turnSlot (see
	// lockTurn); pendingAsks is every unanswered permission/question ask,
	// keyed by toolCallId. Both under mu. hooks tells the server when either
	// changes.
	queuedTurns map[string]context.CancelFunc
	pendingAsks map[string]protocol.PendingAsk
	hooks       Hooks

	// native is the CLI agent session this chat continues (native.go);
	// nativeRemember carries permission_response's remember flag back to a
	// native ask (under mu).
	native         *NativeSessionRef
	nativeRemember map[string]bool
}

// Hooks lets the server react to agent state that isn't a session event:
// a status bar's queued-turn count, and the cross-session pending-asks list.
// Nil funcs are no-ops.
type Hooks struct {
	QueueChanged func()
	AsksChanged  func()
}

func (h Hooks) queueChanged() {
	if h.QueueChanged != nil {
		h.QueueChanged()
	}
}

func (h Hooks) asksChanged() {
	if h.AsksChanged != nil {
		h.AsksChanged()
	}
}

// lockTurn claims the session's turn slot, counting the turn as queued
// while it waits behind the current one. A queued turn can be withdrawn
// (CancelTurn/WithdrawTurn, or ctx ending) before it starts: lockTurn then
// reports turn_completed "cancelled" itself and returns false, and the
// caller must return without touching history. A turn that gets the slot is
// announced with turn_started, the point after which it can't be withdrawn.
func (a *Agent) lockTurn(ctx context.Context, sessionID, turnID string) bool {
	select {
	case a.turnSlot <- struct{}{}:
	default:
		qctx, cancel := context.WithCancel(ctx)
		defer cancel()
		a.mu.Lock()
		a.queuedTurns[turnID] = cancel
		a.mu.Unlock()
		a.hooks.queueChanged()
		got := false
		select {
		case a.turnSlot <- struct{}{}:
			got = true
		case <-qctx.Done():
		}
		a.mu.Lock()
		delete(a.queuedTurns, turnID)
		a.mu.Unlock()
		a.hooks.queueChanged()
		// Both cases can be ready at once; a withdrawn turn must never run.
		if got && qctx.Err() != nil {
			a.unlockTurn()
			got = false
		}
		if !got {
			_ = a.getStream().SendEvent(protocol.EventTurnCompleted, sessionID, turnID, "", a.buildTurnCompletedData("cancelled", protocol.UsageData{}, time.Now(), "", ""))
			return false
		}
	}
	_ = a.getStream().SendEvent(protocol.EventTurnStarted, sessionID, turnID, "", protocol.TurnStartedData{})
	return true
}

func (a *Agent) unlockTurn() { <-a.turnSlot }

// QueuedTurns is how many turns are waiting for this session's current one.
func (a *Agent) QueuedTurns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queuedTurns)
}

// WithdrawTurn cancels turnID only if it's still queued, reporting whether
// it was. Unlike CancelTurn it never stops a turn that already started, so
// a client's "undo send" can't race into cancelling the reply it wanted.
func (a *Agent) WithdrawTurn(turnID string) bool {
	a.mu.Lock()
	cancel, ok := a.queuedTurns[turnID]
	a.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (a *Agent) addPendingAsk(ask protocol.PendingAsk) {
	ask.CreatedAt = time.Now().UTC()
	a.mu.Lock()
	a.pendingAsks[ask.ToolCallID] = ask
	a.mu.Unlock()
	a.hooks.asksChanged()
}

func (a *Agent) removePendingAsk(toolCallID string) {
	a.mu.Lock()
	_, ok := a.pendingAsks[toolCallID]
	delete(a.pendingAsks, toolCallID)
	a.mu.Unlock()
	if ok {
		a.hooks.asksChanged()
	}
}

// PendingAsks returns this session's unanswered asks.
func (a *Agent) PendingAsks() []protocol.PendingAsk {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.PendingAsk, 0, len(a.pendingAsks))
	for _, ask := range a.pendingAsks {
		out = append(out, ask)
	}
	return out
}

func NewAgent(
	stream *protocol.IPCStream,
	provider ModelProvider,
	providerName string,
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
	var toolDefsPlan []tool.ToolDefinition
	for _, t := range tools {
		toolMap[t.Name()] = t
		def := t.Definition()
		toolDefs = append(toolDefs, def)
		if readOnlyTools[t.Name()] {
			toolDefsReadOnly = append(toolDefsReadOnly, def)
			toolDefsPlan = append(toolDefsPlan, def)
		} else if planExtraTools[t.Name()] {
			toolDefsPlan = append(toolDefsPlan, def)
		}
	}
	if ctxMgr == nil {
		ctxMgr = NewContextManager(64000, 4000)
	}

	streams := protocol.NewStreamSet()
	streams.Add(stream)

	return &Agent{
		stream:            streams,
		provider:          provider,
		providerName:      providerName,
		tools:             toolMap,
		toolDefs:          toolDefs,
		toolDefsReadOnly:  toolDefsReadOnly,
		toolDefsPlan:      toolDefsPlan,
		mode:              ModeReadWrite,
		webToolsEnabled:   true,
		permEngine:        engine,
		snapshotStore:     snapStore,
		sessionStore:      sessStore,
		contextManager:    ctxMgr,
		usageStore:        usageStore,
		history:           make([]Message, 0),
		branches:          make(map[string][][]Message),
		permRequests:      make(map[string]chan permDecision),
		permRequestKeys:   make(map[string]string),
		askUserRequests:   make(map[string]chan string),
		activeTurnCancels: make(map[string]context.CancelFunc),
		turnSlot:          make(chan struct{}, 1),
		queuedTurns:       make(map[string]context.CancelFunc),
		pendingAsks:       make(map[string]protocol.PendingAsk),
		nativeRemember:    make(map[string]bool),
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
		a.branches = make(map[string][][]Message)
		return nil
	}

	a.sessionID = state.SessionID
	a.history = state.History
	a.branches = state.Branches
	if a.branches == nil {
		a.branches = make(map[string][][]Message)
	}
	a.nextMsgSeq = state.NextMsgSeq
	a.native = state.Native
	switch Mode(state.Mode) {
	case ModeReadOnly, ModeReadWrite, ModeAutonomous, ModePlan:
		a.mode = Mode(state.Mode)
	default:
		// Empty (never set) or from a session file older than this field —
		// keep the constructor's default (ModeReadWrite) rather than an
		// unrecognized/zero value.
	}

	// Migration: a session persisted before Message.ID existed has none —
	// assign them now, once. Every message this Agent appends from here on
	// gets an ID at append time (nextMessageIDLocked), so this never needs
	// to run twice. Old sessions never have Branches, so there's nothing to
	// migrate there.
	for i := range a.history {
		if a.history[i].ID == "" {
			a.history[i].ID = a.nextMessageIDLocked()
		}
	}
	return nil
}

// nextMessageIDLocked mints the next message ID for this session. Caller
// must already hold a.mu (same convention as session.go's saveLocked/
// loadLocked) — every append site (RunTurn's user message, the two
// assistant-message appends, recordToolResult, EditMessage) already does.
func (a *Agent) nextMessageIDLocked() string {
	a.nextMsgSeq++
	return fmt.Sprintf("m%d", a.nextMsgSeq)
}

func (a *Agent) CurrentSessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// SetStream subscribes another connection to this session, e.g. a second
// device/tab opening a chat that's already live elsewhere, or the same
// session reached over a different transport (unix socket and TCP both
// hitting this process). Every subscribed connection keeps receiving this
// session's events — model_delta/tool_call_requested/permission_request/
// tool_result/turn_completed, and session_updated/etc. — for as long as it
// stays subscribed; nothing displaces an earlier one any more (see
// protocol.StreamSet). Safe to call more than once with the same stream.
func (a *Agent) SetStream(stream *protocol.IPCStream) {
	a.stream.Add(stream)
}

func (a *Agent) getStream() *protocol.StreamSet {
	return a.stream
}

// SetProvider hot-swaps the model/provider a session uses. Takes effect on
// the next turn; an in-flight turn keeps using whichever provider it started
// with. name is the registered provider name (set_provider's providerName)
// stamped onto every assistant Message this provider goes on to generate.
func (a *Agent) SetProvider(p ModelProvider, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
	a.providerName = name
}

func (a *Agent) getProvider() ModelProvider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

func (a *Agent) getProviderName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.providerName
}

// getModelName reports the current provider's active model id, or "" if the
// provider doesn't expose one (see ModelName() implementations).
func (a *Agent) getModelName() string {
	if modelNamer, ok := a.getProvider().(interface{ ModelName() string }); ok {
		return modelNamer.ModelName()
	}
	return ""
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

// SetMode switches a session between the four tiers (SPEC.md §4c). Takes
// effect on the next turn; an in-flight turn keeps whatever tool set and
// permission behavior it started with.
//
// Persisted immediately (not just held in memory) so a peer that connects
// or reconnects after another peer already changed it reads the true
// current value via get_session_history rather than a stale client-side
// cache — the same "server is the source of truth, not whoever reconnects
// last" reasoning applies here as it does everywhere else session state is
// shared across peers.
func (a *Agent) SetMode(m Mode) {
	a.mu.Lock()
	a.mode = m
	sessID := a.sessionID
	a.mu.Unlock()

	if a.sessionStore == nil || sessID == "" {
		return
	}
	// UpdateExisting, not Update: toggling mode on a chat that hasn't been
	// prompted yet must not create its session file — that left an empty,
	// untitled ghost chat in list_sessions for every new chat whose mode was
	// picked before (or instead of) its first message. The first turn's
	// persistCurrentState writes the live mode along with everything else.
	_, _ = a.sessionStore.UpdateExisting(sessID, func(state *SessionState) error {
		if a.isDiscarded() {
			return errSessionDiscarded
		}
		state.Mode = string(m)
		return nil
	})
}

func (a *Agent) Mode() Mode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// SetWebToolsEnabled toggles whether web_search/web_fetch are exposed to the
// model, independent of mode. Takes effect on the next turn.
func (a *Agent) SetWebToolsEnabled(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.webToolsEnabled = enabled
}

func (a *Agent) WebToolsEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.webToolsEnabled
}

// webToolNames is filtered out of activeToolDefs when web tools are
// disabled, regardless of mode.
var webToolNames = map[string]bool{
	"web_search": true,
	"web_fetch":  true,
}

// activeToolDefs returns the tool definitions to hand the model this turn —
// the mode-restricted subset, further filtered by the web-tools toggle.
func (a *Agent) activeToolDefs() []tool.ToolDefinition {
	a.mu.Lock()
	base := a.toolDefsForModeLocked()
	webEnabled := a.webToolsEnabled
	a.mu.Unlock()

	if webEnabled {
		return base
	}
	filtered := make([]tool.ToolDefinition, 0, len(base))
	for _, def := range base {
		if !webToolNames[def.Name] {
			filtered = append(filtered, def)
		}
	}
	return filtered
}

// toolDefsForModeLocked returns the precomputed slice for the current mode.
// Caller must hold a.mu.
func (a *Agent) toolDefsForModeLocked() []tool.ToolDefinition {
	switch a.mode {
	case ModeReadOnly:
		return a.toolDefsReadOnly
	case ModePlan:
		return a.toolDefsPlan
	default: // ModeReadWrite, ModeAutonomous
		return a.toolDefs
	}
}

// isToolAllowedInMode mirrors activeToolDefs' filtering, for the
// defense-in-depth check at dispatch time (a stale/misbehaving provider
// might still request a tool that was withheld from it, or a toggle flipped
// mid-turn).
func (a *Agent) isToolAllowedInMode(name string) bool {
	a.mu.Lock()
	mode := a.mode
	webEnabled := a.webToolsEnabled
	a.mu.Unlock()

	if webToolNames[name] && !webEnabled {
		return false
	}
	switch mode {
	case ModeReadOnly:
		return readOnlyTools[name]
	case ModePlan:
		return readOnlyTools[name] || planExtraTools[name]
	default: // ModeReadWrite, ModeAutonomous
		return true
	}
}

// activeSystemPrompt returns the system prompt to send this turn — Plan
// mode's distinct planning-purpose prompt, DefaultSystemPrompt otherwise.
func (a *Agent) activeSystemPrompt() string {
	if a.Mode() == ModePlan {
		return PlanSystemPrompt
	}
	return DefaultSystemPrompt
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

// CancelTurn stops turnID, whether it's running or still queued.
func (a *Agent) CancelTurn(turnID string) {
	a.mu.Lock()
	cancel, exists := a.activeTurnCancels[turnID]
	if !exists {
		cancel, exists = a.queuedTurns[turnID]
	}
	a.mu.Unlock()

	if exists && cancel != nil {
		cancel()
	}
}

// ActiveTurnCount is how many turns are running on this agent right now.
func (a *Agent) ActiveTurnCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.activeTurnCancels)
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
	if _, ok := a.nativeRemember[toolCallID]; ok {
		a.nativeRemember[toolCallID] = remember
	}
	a.mu.Unlock()

	if remember && cacheKey != "" {
		a.permEngine.RecordDecision(cacheKey, allowed, true)
	}
	a.removePendingAsk(toolCallID)

	if exists {
		ch <- permDecision{allowed: allowed, feedback: feedback}
	}
}

// HandleAskUserAnswer resolves an outstanding ask_user_question tool call
// (the ask_user_answer wire event, correlated by toolCallId, same shape as
// HandlePermissionResponse minus the remember/cache concept, which doesn't
// apply here).
func (a *Agent) HandleAskUserAnswer(toolCallID string, answer string) {
	a.mu.Lock()
	ch, exists := a.askUserRequests[toolCallID]
	delete(a.askUserRequests, toolCallID)
	a.mu.Unlock()
	a.removePendingAsk(toolCallID)

	if exists {
		ch <- answer
	}
}

func (a *Agent) RunTurn(parentCtx context.Context, sessionID, turnID, prompt string) error {
	return a.RunTurnWithTitleGeneration(parentCtx, sessionID, turnID, prompt, true)
}

// RunTurnWithTitleGeneration runs a turn and titles a newly-created chat in
// parallel. Existing callers use RunTurn, whose default is enabled.
func (a *Agent) RunTurnWithTitleGeneration(parentCtx context.Context, sessionID, turnID, prompt string, autoGenerateTitle bool) error {
	// Serialize turns within this session: see turnSlot's doc comment. This
	// must wrap the budget check through the final usageStore.Record write
	// (both happen below) so the check-then-record isn't itself racy. Also
	// serializes against EditMessage/RetryMessage/SwitchBranch on the same
	// Agent — all four mutate a.history/a.branches and must never interleave.
	if !a.lockTurn(parentCtx, sessionID, turnID) {
		return nil
	}
	defer a.unlockTurn()

	startTime := time.Now()
	author := AuthorFromContext(parentCtx)

	turnProvider, err := a.beginTurn(sessionID, turnID, startTime)
	if err != nil {
		return err
	}

	a.mu.Lock()
	isNewChat := len(a.history) == 0
	userMessageID := a.nextMessageIDLocked()
	a.history = append(a.history, Message{
		ID:           userMessageID,
		Role:         "user",
		Content:      prompt,
		AuthorName:   author.Name,
		AuthorAvatar: author.Avatar,
		Attachments:  attachmentsFromContext(parentCtx),
		CreatedAt:    startTime,
	})
	a.mu.Unlock()

	if isNewChat {
		a.titleWG.Add(1)
		go a.runTitleGeneration(parentCtx, sessionID, turnID, prompt, autoGenerateTitle, turnProvider)
	}

	return a.runGenerationLoop(parentCtx, turnID, startTime, author, turnProvider, userMessageID)
}

// beginTurn resolves and binds sessionID, then runs the two checks that can
// fail a turn before any generation starts: no provider configured, or the
// session's spend cap already reached. Either failure emits the same
// turn_completed a normal exit would, so a client always gets exactly one
// terminal event per turn — shared by RunTurn, EditMessage, and
// RetryMessage, which all start a turn the same way.
func (a *Agent) beginTurn(sessionID, turnID string, startTime time.Time) (ModelProvider, error) {
	a.mu.Lock()
	if a.sessionID == "" || a.sessionID != sessionID {
		a.sessionID = sessionID
	}
	a.mu.Unlock()

	turnProvider := a.getProvider()
	if turnProvider == nil {
		_ = a.getStream().SendEvent(protocol.EventTurnCompleted, sessionID, turnID, "", a.buildTurnCompletedData("no_provider", protocol.UsageData{}, startTime, "", ""))
		return nil, fmt.Errorf("session %s has no provider configured — call register_provider and set_provider first", sessionID)
	}

	a.mu.Lock()
	spendCap := a.maxSessionSpendUSD
	a.mu.Unlock()
	if a.usageStore != nil && spendCap > 0 {
		if spent := a.usageStore.SessionTotal(sessionID).CostUSD; spent >= spendCap {
			_ = a.getStream().SendEvent(protocol.EventTurnCompleted, sessionID, turnID, "", a.buildTurnCompletedData("budget_exceeded", protocol.UsageData{}, startTime, "", ""))
			return nil, fmt.Errorf("session %s has already spent $%.4f, at or above its $%.4f cap", sessionID, spent, spendCap)
		}
	}
	return turnProvider, nil
}

// runTitleGeneration runs — and saves/broadcasts — independently of the turn
// it started alongside: a title request is a small, non-streaming call that
// resolves in well under a second, so gating it behind the full turn (which
// may run many tool calls before it's done) would make title generation
// look like it only starts once the answer is already in, instead of racing
// it the way SPEC.md §3.1 describes. Called for a brand-new chat's first
// prompt, and for an edit of a session's very first message (which is, from
// the title's point of view, the same situation).
func (a *Agent) runTitleGeneration(parentCtx context.Context, sessID, turnID, prompt string, autoGenerateTitle bool, turnProvider ModelProvider) {
	// Runs unsupervised in its own goroutine (see the two call sites) — a
	// panic here (e.g. from a misbehaving TitleGenerator) must not take the
	// whole process down along with every other session's connection.
	defer a.titleWG.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("title generation for session %s panicked: %v\n%s", sessID, r, debug.Stack())
		}
	}()
	title := fallbackTitle(prompt)
	if autoGenerateTitle {
		if generator, ok := turnProvider.(TitleGenerator); ok {
			// parentCtx, not a turn-scoped context: that would be cancelled the
			// moment the turn's own RunTurn/EditMessage/RetryMessage call
			// returns, or the user aborts the turn via CancelTurn, either of
			// which would otherwise race this goroutine's own HTTP request and
			// kill it with "context canceled" before it can finish — defeating
			// the whole point of running independently (see above).
			if generated, err := generator.GenerateTitle(parentCtx, prompt); err == nil {
				title = normalizeTitle(generated)
			} else {
				log.Printf("title generation: %v — falling back to a title derived from the prompt", err)
			}
		}
	}
	a.applyGeneratedTitle(sessID, turnID, title)
}

// applyGeneratedTitle saves `title` (if still empty) and broadcasts the
// resulting session_updated — the only place an auto-generated title ever
// reaches a connected client, since it has no client-driven
// set_session_title to piggyback on.
func (a *Agent) applyGeneratedTitle(sessID, turnID, title string) {
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
			Mode:            state.Mode,
		})
	}
}

// persistCurrentState writes this Agent's live history/branches/ID counter
// to the session store, leaving metadata changed through set_session_*
// while this Agent was live intact (see SessionStore.Update for why that
// read-modify-write has to be atomic). Never holds a.mu across the write —
// CancelTurn takes that mutex, and a save must not be able to delay an
// abort. Shared by runGenerationLoop (every turn persists on the way out)
// and SwitchBranch (a branch swap persists immediately, with no turn of its
// own to attach the write to).
func (a *Agent) persistCurrentState() error {
	a.mu.Lock()
	sessID := a.sessionID
	history := append([]Message(nil), a.history...)
	branches := cloneBranches(a.branches)
	nextMsgSeq := a.nextMsgSeq
	mode := a.mode
	native := a.native
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
		state.Branches = branches
		state.NextMsgSeq = nextMsgSeq
		state.Mode = string(mode)
		state.Native = native
		return nil
	})
	if errors.Is(err, errSessionDiscarded) {
		return nil
	}
	return err
}

// cloneBranches deep-copies a branches map so a snapshot taken under a.mu
// (for persistCurrentState) can't be mutated by a later edit/retry/switch
// while the write is still in flight.
func cloneBranches(b map[string][][]Message) map[string][][]Message {
	if b == nil {
		return nil
	}
	out := make(map[string][][]Message, len(b))
	for bp, alts := range b {
		cp := make([][]Message, len(alts))
		for i, alt := range alts {
			cp[i] = append([]Message(nil), alt...)
		}
		out[bp] = cp
	}
	return out
}

// runGenerationLoop is the shared core of every turn — RunTurn (after
// appending the user's new message), EditMessage (after truncating to the
// edited message), and RetryMessage (after truncating to the retried
// point) all converge here: run model round-trips against whatever
// a.history currently is, executing tool calls in between, until the turn
// finishes, is cancelled, or the provider fails, then persist and report
// turn_completed exactly once.
func (a *Agent) runGenerationLoop(parentCtx context.Context, turnID string, startTime time.Time, author Author, turnProvider ModelProvider, userMessageID string) error {
	sessionID := a.CurrentSessionID()
	var lastMessageID string

	turnCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	a.mu.Lock()
	a.activeTurnCancels[turnID] = cancel
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.activeTurnCancels, turnID)
		a.mu.Unlock()
		if a.snapshotStore != nil {
			a.snapshotStore.CloseTurn(turnID)
		}
	}()

	// Write this turn's starting point to disk before the model is asked
	// anything at all. Every way out of this function below persists again
	// (the final save), so this first write is what makes the prompt/edit/
	// retry itself durable against a cancel, a provider that never responds,
	// or the process being killed before the turn ends — and it's what makes
	// the session file exist on a first message, which set_session_title
	// requires (SPEC.md §3.1) and which used to only appear once a turn had
	// already completed successfully.
	if err := a.persistCurrentState(); err != nil {
		// Reported without a turnId on purpose: an error carrying this turn's id
		// would make the client tear down its streaming state while the model is
		// still answering. The save below reports it properly if it's still
		// failing by then.
		_ = a.getStream().SendEvent(protocol.EventError, sessionID, "", "", protocol.ErrorData{
			Message: fmt.Sprintf("session %s could not be saved yet (%v) — this turn may be lost if the server stops", sessionID, err),
		})
	}

	var (
		totalUsage     protocol.UsageData
		roundTripUsage protocol.UsageData
		finishReason   string
		streamErr      error
		cutShort       bool
		assistantText  string
		toolCalls      []ToolCall
		roundTripStart time.Time

		// Native agents only (native.go): calls the agent already ran,
		// which are recorded but never executed here, and the turn's
		// NativeTurn, whose SessionID is read back after the stream.
		segCalls []ToolCall
		nt       *NativeTurn
	)
	_, isNative := turnProvider.(NativeAgent)

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
		assistantText, toolCalls, segCalls = "", nil, nil
		roundTripStart = time.Now()
		roundTripUsage = protocol.UsageData{}
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
		activeContext := withAttachmentNotes(a.contextManager.PrepareHistory(a.history))
		a.mu.Unlock()

		streamCtx, toolDefs, systemPrompt := turnCtx, a.activeToolDefs(), a.activeSystemPrompt()
		if isNative {
			a.mu.Lock()
			hist := append([]Message(nil), a.history...)
			a.mu.Unlock()
			nt = a.newNativeTurn(sessionID, turnID, a.getProviderName(), hist)
			streamCtx = WithNativeTurn(turnCtx, nt)
			toolDefs, systemPrompt = nil, ""
		} else {
			activeContext = flattenNativeTools(activeContext)
		}

		// flushSegment stores a native round's output so far as its own
		// assistant message, so a tool result lands after the call that
		// produced it rather than after the whole turn's text.
		segStart := roundTripStart
		flushSegment := func() {
			if assistantText == "" && reasoningText == "" && len(segCalls) == 0 {
				return
			}
			providerName, modelName := a.getProviderName(), a.getModelName()
			a.mu.Lock()
			lastMessageID = a.nextMessageIDLocked()
			a.history = append(a.history, Message{
				ID:           lastMessageID,
				Role:         "assistant",
				Content:      assistantText,
				Reasoning:    reasoningText,
				ToolCalls:    segCalls,
				Provider:     providerName,
				Model:        modelName,
				CreatedAt:    time.Now(),
				GenerationMs: time.Since(segStart).Milliseconds(),
			})
			a.mu.Unlock()
			assistantText, reasoningText, segCalls = "", "", nil
			segStart = time.Now()
		}

		chunks, errs := turnProvider.StreamTurn(streamCtx, activeContext, toolDefs, systemPrompt)

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
						if chunk.ToolCall.Native {
							segCalls = append(segCalls, *chunk.ToolCall)
						} else {
							toolCalls = append(toolCalls, *chunk.ToolCall)
						}
						_ = a.getStream().SendEvent(protocol.EventToolCallRequested, a.sessionID, turnID, chunk.ToolCall.ID, protocol.ToolCallData{
							Name:           chunk.ToolCall.Name,
							Args:           chunk.ToolCall.Args,
							NativeToolInfo: chunk.ToolCall.NativeToolInfo,
						})
					}
				case StreamChunkToolResult:
					if r := chunk.ToolResult; r != nil {
						flushSegment()
						a.mu.Lock()
						a.history = append(a.history, Message{
							ID:         a.nextMessageIDLocked(),
							Role:       "tool",
							Content:    r.Output + r.Error,
							ToolCallID: r.ToolCallID,
							Diffs:      r.Diffs,
						})
						a.mu.Unlock()
						_ = a.getStream().SendEvent(protocol.EventToolResult, a.sessionID, turnID, r.ToolCallID, protocol.ToolResultData{
							ToolName: r.Name,
							Output:   r.Output,
							Error:    r.Error,
							Diffs:    r.Diffs,
						})
					}
				case StreamChunkUsage:
					if chunk.Usage != nil {
						totalUsage.InputTokens += chunk.Usage.InputTokens
						totalUsage.OutputTokens += chunk.Usage.OutputTokens
						totalUsage.TotalTokens += chunk.Usage.TotalTokens
						// This round-trip's own usage, not the whole turn's — a
						// provider's InputTokens is already the full conversation
						// resent as this request's input, so it's naturally "tokens
						// in up to this point" with no separate accumulation needed;
						// OutputTokens is naturally just this round-trip's own reply.
						roundTripUsage.InputTokens += chunk.Usage.InputTokens
						roundTripUsage.OutputTokens += chunk.Usage.OutputTokens
						roundTripUsage.TotalTokens += chunk.Usage.TotalTokens
						if chunk.Usage.CostKnown {
							totalUsage.CostUSD += chunk.Usage.CostUSD
							totalUsage.CostKnown = true
							roundTripUsage.CostUSD += chunk.Usage.CostUSD
							roundTripUsage.CostKnown = true
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
		if assistantText != "" || len(toolCalls) > 0 || len(segCalls) > 0 || (isNative && reasoningText != "") {
			providerName, modelName := a.getProviderName(), a.getModelName()
			usage := roundTripUsage
			if !usage.CostKnown && modelName != "" {
				usage.CostUSD = EstimateCostUSD(modelName, usage)
			}
			a.mu.Lock()
			lastMessageID = a.nextMessageIDLocked()
			a.history = append(a.history, Message{
				ID:           lastMessageID,
				Role:         "assistant",
				Content:      assistantText,
				Reasoning:    reasoningText,
				ToolCalls:    append(toolCalls, segCalls...),
				Provider:     providerName,
				Model:        modelName,
				CreatedAt:    time.Now(),
				GenerationMs: time.Since(segStart).Milliseconds(),
				Usage:        &usage,
			})
			a.mu.Unlock()
		} else if isNative {
			// Ended right after a tool result: the usage (reported once, at
			// the end of a native turn) goes on the turn's last text message.
			a.attachUsage(lastMessageID, roundTripUsage)
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
			mode := a.Mode()
			if !a.isToolAllowedInMode(tc.Name) {
				a.recordToolResult(turnID, tc, "", fmt.Sprintf("tool %s is unavailable in the current mode", tc.Name))
				continue
			}

			allowed := true
			var denyFeedback string
			if mode == ModeAutonomous {
				// Autonomous: every permission check is bypassed outright,
				// permission.Engine is never consulted (SPEC.md §4c).
			} else if toolImpl.RequiresPermission(tc.Args) {
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

				var decision permission.Decision
				var cacheKey string
				if mode == ModeReadWrite && tc.Name == "bash" {
					// Read/Write+Prompts: bash always asks, bypassing the
					// engine's safe-command auto-allowlist. cacheKey is left
					// empty so a "remember" answer here is never persisted
					// (HandlePermissionResponse only calls RecordDecision
					// when cacheKey != "") — otherwise a remembered bash
					// command would silently auto-allow itself again later,
					// including in other modes.
					decision, cacheKey = permission.DecisionAsk, ""
				} else {
					decision, cacheKey = a.permEngine.Evaluate(tc.Name, target)
				}

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

					req := protocol.PermissionRequestData{
						ToolName:    tc.Name,
						Description: fmt.Sprintf("%s on %s", tc.Name, string(tc.Args)),
						RequiresAsk: true,
						CacheKey:    cacheKey,
						Diff:        computedDiff,
					}
					a.addPendingAsk(protocol.PendingAsk{Kind: "permission", SessionID: sessionID, TurnID: turnID, ToolCallID: tc.ID, PermissionRequestData: &req})
					_ = a.getStream().SendEvent(protocol.EventPermissionRequest, a.sessionID, turnID, tc.ID, req)

					select {
					case <-turnCtx.Done():
						a.mu.Lock()
						delete(a.permRequests, tc.ID)
						delete(a.permRequestKeys, tc.ID)
						a.mu.Unlock()
						a.removePendingAsk(tc.ID)
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

			toolCallID := tc.ID
			execCtx := tool.ExecutionContext{
				SessionID:     a.sessionID,
				TurnID:        turnID,
				ToolCallID:    toolCallID,
				SnapshotStore: a.snapshotStore,
				Stream:        a.getStream(),
				AuthorName:    author.Name,
				AuthorEmail:   author.Email,
				ExtraReadPaths: func() []string {
					a.mu.Lock()
					defer a.mu.Unlock()
					return a.sessionAttachmentsLocked()
				}(),
				AskUser: func(question string, options []string) (string, error) {
					return a.askUserQuestion(turnCtx, sessionID, turnID, toolCallID, question, options)
				},
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
	if (cutShort || streamErr != nil) && (assistantText != "" || len(toolCalls) > 0 || len(segCalls) > 0) {
		providerName, modelName := a.getProviderName(), a.getModelName()
		usage := roundTripUsage
		if !usage.CostKnown && modelName != "" {
			usage.CostUSD = EstimateCostUSD(modelName, usage)
		}
		a.mu.Lock()
		lastMessageID = a.nextMessageIDLocked()
		a.history = append(a.history, Message{
			ID:           lastMessageID,
			Role:         "assistant",
			Content:      assistantText,
			ToolCalls:    append(toolCalls, segCalls...),
			Provider:     providerName,
			Model:        modelName,
			CreatedAt:    time.Now(),
			GenerationMs: time.Since(roundTripStart).Milliseconds(),
			Usage:        &usage,
		})
		a.mu.Unlock()
	}

	if nt != nil && nt.SessionID != "" {
		a.mu.Lock()
		cursor := ""
		if n := len(a.history); n > 0 {
			cursor = a.history[n-1].ID
		}
		a.native = &NativeSessionRef{Provider: a.providerName, SessionID: nt.SessionID, Cursor: cursor, CostUSD: nt.SessionCostUSD}
		a.mu.Unlock()
	}

	// The one save every way out of this function goes through — normal
	// completion, a cancel, a provider failure. Not discarded: the turn's
	// content exists nowhere else, so a failed write means the conversation the
	// user just had is not on disk, and saying nothing would report a turn that
	// can't be reloaded as an ordinary success.
	if saveErr := a.persistCurrentState(); saveErr != nil {
		_ = a.getStream().SendEvent(protocol.EventError, a.sessionID, turnID, "", protocol.ErrorData{
			Message: fmt.Sprintf("session %s could not be saved (%v) — this turn will not survive a restart", a.sessionID, saveErr),
		})
	}

	// A provider failure has no finishReason to report — the dispatcher emits
	// the `error` frame for it (§3.2 has no "error" finish reason), so
	// turn_completed is reserved for the reasons it does define.
	if finishReason != "" {
		_ = a.getStream().SendEvent(protocol.EventTurnCompleted, a.sessionID, turnID, "", a.buildTurnCompletedData(finishReason, totalUsage, startTime, userMessageID, lastMessageID))
	}

	if streamErr != nil {
		return streamErr
	}
	if cutShort {
		return turnCtx.Err()
	}
	return nil
}

// EditMessage replaces a past user message with new content and regenerates
// everything after it. The message's old content and whatever was generated
// after it aren't deleted — they're stashed under Branches (keyed by the ID
// of the message right before it) and stay recoverable via SwitchBranch,
// since that content may already have cost real money to generate. Only a
// message currently reachable from the active history can be targeted; a
// message buried in an inactive branch needs SwitchBranch first (walking
// the tree one hop at a time composes correctly without a full recursive
// lookup — see the plan's scope note).
func (a *Agent) EditMessage(parentCtx context.Context, sessionID, turnID, messageID, content string, autoGenerateTitle bool) error {
	if !a.lockTurn(parentCtx, sessionID, turnID) {
		return nil
	}
	defer a.unlockTurn()

	startTime := time.Now()
	author := AuthorFromContext(parentCtx)

	turnProvider, err := a.beginTurn(sessionID, turnID, startTime)
	if err != nil {
		return err
	}

	a.mu.Lock()
	idx, found := indexOfMessageLocked(a.history, messageID)
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("message %s: %w", messageID, ErrMessageNotFound)
	}
	if a.history[idx].Role != "user" {
		a.mu.Unlock()
		return fmt.Errorf("message %s: %w", messageID, ErrMessageNotEditable)
	}
	branchPoint := ""
	if idx > 0 {
		branchPoint = a.history[idx-1].ID
	}
	if a.branches == nil {
		a.branches = make(map[string][][]Message)
	}
	a.branches[branchPoint] = append(a.branches[branchPoint], append([]Message(nil), a.history[idx:]...))
	isNewChat := idx == 0
	userMessageID := a.nextMessageIDLocked()
	attachments := a.history[idx].Attachments // an edit rewrites the text, not what was attached
	a.history = append(a.history[:idx:idx], Message{
		ID:           userMessageID,
		Role:         "user",
		Content:      content,
		AuthorName:   author.Name,
		AuthorAvatar: author.Avatar,
		Attachments:  attachments,
		CreatedAt:    startTime,
	})
	a.mu.Unlock()

	if isNewChat {
		// Editing the very first message leaves the session with no history
		// again, same as a brand-new chat — and a title generated from the
		// original prompt no longer describes what's actually being asked, so
		// this re-runs title generation exactly like a fresh chat would.
		// applyGeneratedTitle's own "only fills in an empty title" rule (SPEC.md
		// §3.1) still applies unchanged: a session that already got a real
		// title (its own set_session_title, or a previous generation) keeps it.
		a.titleWG.Add(1)
		go a.runTitleGeneration(parentCtx, sessionID, turnID, content, autoGenerateTitle, turnProvider)
	}

	return a.runGenerationLoop(parentCtx, turnID, startTime, author, turnProvider, userMessageID)
}

// RetryMessage discards a past assistant message and regenerates a fresh
// response from the same point — everything from that message onward is
// stashed under Branches (recoverable via SwitchBranch), same as
// EditMessage. messageID may name any assistant message in the active
// history, not just the last one: retrying an earlier round-trip inside a
// multi-tool-call turn discards that round-trip and everything after it too.
//
// This never undoes real side effects a discarded branch's tool calls already
// made (files written, commands run) — only conversation history moves.
// restore_snapshot (SPEC.md §3.1) is the tool for reverting the filesystem
// itself, and is orthogonal to this.
func (a *Agent) RetryMessage(parentCtx context.Context, sessionID, turnID, messageID string) error {
	if !a.lockTurn(parentCtx, sessionID, turnID) {
		return nil
	}
	defer a.unlockTurn()

	startTime := time.Now()
	author := AuthorFromContext(parentCtx)

	turnProvider, err := a.beginTurn(sessionID, turnID, startTime)
	if err != nil {
		return err
	}

	a.mu.Lock()
	idx, found := indexOfMessageLocked(a.history, messageID)
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("message %s: %w", messageID, ErrMessageNotFound)
	}
	if a.history[idx].Role != "assistant" {
		a.mu.Unlock()
		return fmt.Errorf("message %s: %w", messageID, ErrMessageNotRetryable)
	}
	branchPoint := ""
	userMessageID := ""
	if idx > 0 {
		branchPoint = a.history[idx-1].ID
		userMessageID = branchPoint
	}
	if a.branches == nil {
		a.branches = make(map[string][][]Message)
	}
	a.branches[branchPoint] = append(a.branches[branchPoint], append([]Message(nil), a.history[idx:]...))
	a.history = a.history[:idx:idx]
	a.mu.Unlock()

	return a.runGenerationLoop(parentCtx, turnID, startTime, author, turnProvider, userMessageID)
}

// SwitchBranch makes the branch containing messageID the active one,
// stashing whatever suffix was active at that branch point back into
// Branches in its place. Pure navigation — no model call, no cost. Blocks
// on turnSlot so it never races a concurrently in-flight turn on the same
// session; it simply waits for one to finish rather than switching out the
// history a turn is actively appending to.
//
// messageID may be any message within a stashed alternate, not just its
// first one. Only branches reachable from the current active history can be
// switched to — a branch nested behind another branch point becomes
// reachable only after switching to that outer one first, which is just
// walking the (implicit) tree one hop at a time.
func (a *Agent) SwitchBranch(sessionID, messageID string) error {
	a.turnSlot <- struct{}{}
	defer a.unlockTurn()

	a.mu.Lock()
	if a.sessionID == "" || a.sessionID != sessionID {
		a.sessionID = sessionID
	}

	branchPoint, altIdx, found := findBranchLocked(a.branches, a.history, messageID)
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("message %s: %w", messageID, ErrMessageNotFound)
	}

	branchStart := 0
	if branchPoint != "" {
		idx, ok := indexOfMessageLocked(a.history, branchPoint)
		if !ok {
			// findBranchLocked only ever returns a branchPoint it already
			// confirmed is reachable from a.history, so this can't happen.
			a.mu.Unlock()
			return fmt.Errorf("message %s: %w", messageID, ErrMessageNotFound)
		}
		branchStart = idx + 1
	}

	currentSuffix := append([]Message(nil), a.history[branchStart:]...)
	newSuffix := a.branches[branchPoint][altIdx]
	a.branches[branchPoint][altIdx] = currentSuffix
	a.history = append(a.history[:branchStart:branchStart], newSuffix...)
	a.mu.Unlock()

	return a.persistCurrentState()
}

// indexOfMessageLocked finds id's position in history. Caller must already
// hold a.mu when history is the live a.history.
func indexOfMessageLocked(history []Message, id string) (int, bool) {
	for i, m := range history {
		if m.ID == id {
			return i, true
		}
	}
	return 0, false
}

// findBranchLocked locates which stashed alternate contains targetID,
// restricted to branch points currently reachable from history (bp == ""
// — the very start of the session — is always reachable; anything else
// must itself be an ID in history). Caller must already hold a.mu.
func findBranchLocked(branches map[string][][]Message, history []Message, targetID string) (branchPoint string, altIndex int, found bool) {
	for bp, alts := range branches {
		if bp != "" {
			if _, ok := indexOfMessageLocked(history, bp); !ok {
				continue
			}
		}
		for i, alt := range alts {
			for _, m := range alt {
				if m.ID == targetID {
					return bp, i, true
				}
			}
		}
	}
	return "", 0, false
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
func (a *Agent) buildTurnCompletedData(finishReason string, usage protocol.UsageData, startTime time.Time, userMessageID, messageID string) protocol.TurnCompletedData {
	a.mu.Lock()
	sessionID := a.sessionID
	historySnapshot := a.history
	a.mu.Unlock()

	// A provider (e.g. a CLI subprocess) may already report an authoritative
	// cost; only fall back to the pricing table when it hasn't (CostKnown
	// distinguishes "no cost reported" from "provider reported a real $0").
	if !usage.CostKnown {
		if modelName := a.getModelName(); modelName != "" {
			usage.CostUSD = EstimateCostUSD(modelName, usage)
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
		FinishReason:  finishReason,
		SessionID:     sessionID,
		UserMessageID: userMessageID,
		MessageID:     messageID,
		Usage:         &usage,
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
		ID:         a.nextMessageIDLocked(),
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
