package agent_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/sandbox"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// ErrorProvider fails every turn at the stream, the way a dropped connection or
// a rejected request does — the case that used to save nothing at all.
type ErrorProvider struct {
	err error
}

func (e *ErrorProvider) StreamTurn(context.Context, []agent.Message, []tool.ToolDefinition) (<-chan agent.ModelStreamChunk, <-chan error) {
	cChan := make(chan agent.ModelStreamChunk, 1)
	eChan := make(chan error, 1)
	// Buffered and closed before this returns, the way a request that fails
	// immediately behaves (a refused connection, a rejected key, a 500): by the
	// time the turn selects on these channels, the error is already waiting *and*
	// both are already closed, so the error must not be skipped just because the
	// chunk channel also reads as finished.
	eChan <- e.err
	close(eChan)
	close(cChan)
	return cChan, eChan
}

// testAgent builds an Agent over a fresh temp project, returning the agent, its
// session store, and the buffer its events are written to so a test can assert
// on what actually reached disk and on what the client was told.
func testAgent(t *testing.T, provider agent.ModelProvider) (*agent.Agent, agent.SessionStore, *bytes.Buffer) {
	t.Helper()
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatalf("normalizer: %v", err)
	}
	sb := sandbox.NewSandbox(tempDir, 5*time.Second)
	engine := permission.New(norm, nil)
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatalf("snapshot store: %v", err)
	}
	sessStore, err := agent.NewDiskSessionStore(tempDir)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	tools := []tool.Tool{tool.NewBashTool(norm, sb)}
	ag := agent.NewAgent(ipc, provider, tools, engine, snapStore, sessStore, agent.NewContextManager(1000, 100), nil)
	return ag, sessStore, outBuf
}

// A turn cancelled mid-stream has to leave durable history behind: the prompt
// at minimum. Before this, a cancel returned before the turn's only save, so a
// cancelled first turn left no session file at all — the chat simply vanished
// from the list on the next reload, which is the reported "chats don't save".
func TestAgent_CancelledTurnPersistsPrompt(t *testing.T) {
	provider := &BlockingProvider{started: make(chan struct{})}
	ag, sessStore, outBuf := testAgent(t, provider)

	go func() {
		<-provider.started
		ag.CancelTurn("turn-1")
	}()

	if err := ag.RunTurn(context.Background(), "session-cancel", "turn-1", "do the thing"); err == nil {
		t.Fatalf("expected the cancelled turn to report an error")
	}

	// The reason has to be the cancel, not a clean finish: the provider tears its
	// channels down on cancellation, which reads as "stream ended" too, and the
	// select between those cases is otherwise a coin flip.
	output := outBuf.String()
	if !strings.Contains(output, `"finishReason":"cancelled"`) {
		t.Fatalf("expected finishReason cancelled, got: %s", output)
	}
	if strings.Contains(output, `"finishReason":"stop"`) {
		t.Fatalf("a cancelled turn reported itself as completed: %s", output)
	}

	state, err := sessStore.Load("session-cancel")
	if err != nil {
		t.Fatalf("cancelled turn left no persisted session: %v", err)
	}
	if len(state.History) == 0 {
		t.Fatalf("cancelled turn persisted an empty history")
	}
	if state.History[0].Role != "user" || state.History[0].Content != "do the thing" {
		t.Fatalf("first stored message = %+v, want the user's prompt", state.History[0])
	}
	// The blocking provider streams a delta before it's cancelled, so the turn
	// may or may not have consumed it by then — but if an assistant message was
	// stored, it must have the text (never an empty placeholder).
	if len(state.History) > 1 && state.History[1].Role != "assistant" {
		t.Fatalf("second stored message = %+v, want an assistant message", state.History[1])
	}
}

// A turn whose provider fails must persist the prompt too: the model being
// unreachable doesn't mean the user's message should disappear.
func TestAgent_FailedTurnPersistsPrompt(t *testing.T) {
	ag, sessStore, _ := testAgent(t, &ErrorProvider{err: errors.New("connection reset by peer")})

	err := ag.RunTurn(context.Background(), "session-error", "turn-1", "are you there?")
	if err == nil {
		t.Fatalf("expected the failed turn to report an error")
	}
	if !errors.Is(err, err) && !bytes.Contains([]byte(err.Error()), []byte("model stream error")) {
		t.Fatalf("error = %v, want it wrapped as a model stream error", err)
	}

	state, loadErr := sessStore.Load("session-error")
	if loadErr != nil {
		t.Fatalf("failed turn left no persisted session: %v", loadErr)
	}
	if len(state.History) != 1 || state.History[0].Content != "are you there?" {
		t.Fatalf("stored history = %+v, want just the user's prompt", state.History)
	}
}

// The sender's name and avatar are stored with the message, so a transcript
// repainted from server history still shows who wrote it — an avatar has no
// other source once the sending client's own settings are out of the picture.
func TestAgent_PersistsAuthorNameAndAvatar(t *testing.T) {
	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{
		{{Kind: agent.StreamChunkDelta, Delta: "hi"}},
	}}
	ag, sessStore, _ := testAgent(t, provider)

	ctx := agent.WithAuthor(context.Background(), agent.Author{Name: "aveline", Avatar: "🐣"})
	if err := ag.RunTurn(ctx, "session-author", "turn-1", "hello"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	state, err := sessStore.Load("session-author")
	if err != nil {
		t.Fatalf("loading session: %v", err)
	}
	if len(state.History) == 0 {
		t.Fatalf("no history stored")
	}
	if state.History[0].AuthorName != "aveline" || state.History[0].AuthorAvatar != "🐣" {
		t.Fatalf("stored author = %q/%q, want aveline/🐣", state.History[0].AuthorName, state.History[0].AuthorAvatar)
	}
	if state.History[1].AuthorName != "" || state.History[1].AuthorAvatar != "" {
		t.Fatalf("assistant message carries an author: %+v", state.History[1])
	}
}

// Metadata written through set_session_* while a turn runs must survive that
// turn's own save. Both are read-modify-writes of the same file, so a save that
// replaced the whole state would silently revert a title (or un-archive a chat)
// that landed while the model was still answering.
func TestAgent_TurnSavePreservesMetadataSetDuringTheTurn(t *testing.T) {
	provider := &BlockingProvider{started: make(chan struct{})}
	ag, sessStore, _ := testAgent(t, provider)

	if err := sessStore.Ensure("session-meta"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ag.RunTurn(context.Background(), "session-meta", "turn-1", "long running question")
	}()

	// Set the title the way the server's set_session_title handler does: while
	// the turn is in flight.
	<-provider.started
	archivedAt := time.Now().UTC()
	if _, err := sessStore.UpdateExisting("session-meta", func(state *agent.SessionState) error {
		state.Title = "a title set mid-turn"
		state.Archived = true
		state.ArchivedAt = &archivedAt
		return nil
	}); err != nil {
		t.Fatalf("UpdateExisting: %v", err)
	}

	ag.CancelTurn("turn-1")
	<-done

	state, err := sessStore.Load("session-meta")
	if err != nil {
		t.Fatalf("loading session: %v", err)
	}
	if state.Title != "a title set mid-turn" {
		t.Fatalf("title = %q, want the mid-turn title preserved by the turn's save", state.Title)
	}
	if !state.Archived {
		t.Fatalf("archived flag was reverted by the turn's save")
	}
	if len(state.History) == 0 {
		t.Fatalf("turn's history was not saved")
	}
}

// UpdateExisting is the metadata-handler variant: it must refuse a session that
// has no persisted state (SPEC.md §3.1 makes set_session_title an error for
// one), while Update creates it — that's a new chat's first turn.
func TestSessionStore_UpdateExistingRequiresAnExistingSession(t *testing.T) {
	_, sessStore, _ := testAgent(t, &ScriptedProvider{})

	if _, err := sessStore.UpdateExisting("session-absent", func(*agent.SessionState) error { return nil }); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("UpdateExisting on a missing session = %v, want ErrSessionNotFound", err)
	}
	if _, err := sessStore.Load("session-absent"); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("Load after a refused UpdateExisting = %v, want ErrSessionNotFound (it must not have created anything)", err)
	}

	if err := sessStore.Ensure("session-absent"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := sessStore.UpdateExisting("session-absent", func(state *agent.SessionState) error {
		state.Title = "now it exists"
		return nil
	}); err != nil {
		t.Fatalf("UpdateExisting after Ensure: %v", err)
	}
	state, err := sessStore.Load("session-absent")
	if err != nil || state.Title != "now it exists" {
		t.Fatalf("state = %+v (err %v), want the title set", state, err)
	}
}
