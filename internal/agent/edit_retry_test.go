package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/agent"
	"yoakeland/internal/normalizer"
	"yoakeland/internal/permission"
	"yoakeland/internal/protocol"
	"yoakeland/internal/snapshot"
	"yoakeland/internal/tool"
)

// TestAgent_EditMessage_StashesOldSuffixAndIsRecoverable covers the core
// promise of edit_message: the old content and everything generated after it
// aren't deleted, just archived under Branches — and switch_branch can bring
// them back, swapping the edited branch out in their place.
func TestAgent_EditMessage_StashesOldSuffixAndIsRecoverable(t *testing.T) {
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "first response"}},
			{{Kind: agent.StreamChunkDelta, Delta: "second response"}},
		},
	}
	ag, sessStore, _ := testAgent(t, provider)
	ctx := context.Background()

	if err := ag.RunTurn(ctx, "s1", "t1", "hello"); err != nil {
		t.Fatalf("initial turn failed: %v", err)
	}

	state, err := sessStore.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(state.History))
	}
	origUserID := state.History[0].ID
	origAssistantID := state.History[1].ID
	if origUserID == "" || origAssistantID == "" {
		t.Fatalf("expected assigned message IDs, got %+v", state.History)
	}

	if err := ag.EditMessage(ctx, "s1", "t2", origUserID, "hello edited", false); err != nil {
		t.Fatalf("edit failed: %v", err)
	}

	state, err = sessStore.Load("s1")
	if err != nil {
		t.Fatalf("load after edit: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("expected 2 messages after edit, got %d", len(state.History))
	}
	if state.History[0].Content != "hello edited" {
		t.Fatalf("expected edited content, got %q", state.History[0].Content)
	}
	if state.History[1].Content != "second response" {
		t.Fatalf("expected a freshly regenerated response, got %q", state.History[1].Content)
	}
	newUserID := state.History[0].ID
	if newUserID == origUserID {
		t.Fatalf("an edited message should get a fresh ID")
	}

	alts, ok := state.Branches[""]
	if !ok || len(alts) != 1 {
		t.Fatalf("expected 1 stashed alternate at the root branch point, got %+v", state.Branches)
	}
	if len(alts[0]) != 2 || alts[0][0].ID != origUserID || alts[0][0].Content != "hello" || alts[0][1].Content != "first response" {
		t.Fatalf("stashed alternate doesn't match the old suffix: %+v", alts[0])
	}

	// Switch back to the original branch — no model call, and the edited
	// branch must itself become recoverable in its place.
	if err := ag.SwitchBranch("s1", origUserID); err != nil {
		t.Fatalf("switch_branch failed: %v", err)
	}
	state, err = sessStore.Load("s1")
	if err != nil {
		t.Fatalf("load after switch: %v", err)
	}
	if state.History[0].Content != "hello" || state.History[1].Content != "first response" {
		t.Fatalf("switch_branch didn't restore the original suffix: %+v", state.History)
	}
	alts, ok = state.Branches[""]
	if !ok || len(alts) != 1 || alts[0][0].ID != newUserID {
		t.Fatalf("expected the edited branch to now be the stashed alternate: %+v", state.Branches)
	}
}

// TestAgent_RetryMessage_StashesOldSuffixAndRegenerates covers retry's
// version of the same guarantee, for the assistant-role path.
func TestAgent_RetryMessage_StashesOldSuffixAndRegenerates(t *testing.T) {
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "first response"}},
			{{Kind: agent.StreamChunkDelta, Delta: "second response"}},
		},
	}
	ag, sessStore, _ := testAgent(t, provider)
	ctx := context.Background()

	if err := ag.RunTurn(ctx, "s2", "t1", "hello"); err != nil {
		t.Fatalf("initial turn failed: %v", err)
	}
	state, err := sessStore.Load("s2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID := state.History[0].ID
	assistantID := state.History[1].ID

	if err := ag.RetryMessage(ctx, "s2", "t2", assistantID); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	state, err = sessStore.Load("s2")
	if err != nil {
		t.Fatalf("load after retry: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("expected 2 messages after retry, got %d", len(state.History))
	}
	if state.History[0].ID != userID {
		t.Fatalf("retry must not touch the user message")
	}
	if state.History[1].Content != "second response" {
		t.Fatalf("expected a freshly regenerated response, got %q", state.History[1].Content)
	}
	alts, ok := state.Branches[userID]
	if !ok || len(alts) != 1 || alts[0][0].ID != assistantID || alts[0][0].Content != "first response" {
		t.Fatalf("expected the discarded response stashed under the user message's branch point: %+v", state.Branches)
	}
}

// TestAgent_TurnCompletedReportsMessageIDs guards the fix for the
// production "message not found" bug: a client must be able to learn a
// message's real id from turn_completed itself — the message it just
// watched stream in — without a follow-up get_session_history round trip
// that could race a fast retry/edit click.
func TestAgent_TurnCompletedReportsMessageIDs(t *testing.T) {
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "first response"}},
			{{Kind: agent.StreamChunkDelta, Delta: "edited response"}},
			{{Kind: agent.StreamChunkDelta, Delta: "retried response"}},
		},
	}
	ag, sessStore, outBuf := testAgent(t, provider)
	ctx := context.Background()

	if err := ag.RunTurn(ctx, "s7", "t1", "hello"); err != nil {
		t.Fatalf("initial turn: %v", err)
	}
	state, err := sessStore.Load("s7")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID, assistantID := state.History[0].ID, state.History[1].ID

	data := lastTurnCompleted(t, outBuf.String())
	if data.UserMessageID != userID {
		t.Fatalf("expected turn_completed.userMessageId %q, got %q", userID, data.UserMessageID)
	}
	if data.MessageID != assistantID {
		t.Fatalf("expected turn_completed.messageId %q, got %q", assistantID, data.MessageID)
	}

	if err := ag.EditMessage(ctx, "s7", "t2", userID, "hello edited", false); err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	state, err = sessStore.Load("s7")
	if err != nil {
		t.Fatalf("load after edit: %v", err)
	}
	newUserID, newAssistantID := state.History[0].ID, state.History[1].ID
	data = lastTurnCompleted(t, outBuf.String())
	if data.UserMessageID != newUserID {
		t.Fatalf("expected the edit's own new id %q, got %q", newUserID, data.UserMessageID)
	}
	if data.MessageID != newAssistantID {
		t.Fatalf("expected the regenerated reply's id %q, got %q", newAssistantID, data.MessageID)
	}

	if err := ag.RetryMessage(ctx, "s7", "t3", newAssistantID); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	state, err = sessStore.Load("s7")
	if err != nil {
		t.Fatalf("load after retry: %v", err)
	}
	retriedAssistantID := state.History[1].ID
	data = lastTurnCompleted(t, outBuf.String())
	if data.UserMessageID != newUserID {
		t.Fatalf("expected retry to report the existing user message it followed (%q), got %q", newUserID, data.UserMessageID)
	}
	if data.MessageID != retriedAssistantID {
		t.Fatalf("expected the freshly retried reply's id %q, got %q", retriedAssistantID, data.MessageID)
	}
}

func TestAgent_EditMessage_RejectsWrongRoleOrMissing(t *testing.T) {
	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "reply"}}}}
	ag, sessStore, _ := testAgent(t, provider)
	ctx := context.Background()
	if err := ag.RunTurn(ctx, "s3", "t1", "hello"); err != nil {
		t.Fatalf("initial turn failed: %v", err)
	}
	state, err := sessStore.Load("s3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assistantID := state.History[1].ID

	if err := ag.EditMessage(ctx, "s3", "t2", assistantID, "nope", false); !errors.Is(err, agent.ErrMessageNotEditable) {
		t.Fatalf("expected ErrMessageNotEditable editing an assistant message, got %v", err)
	}
	if err := ag.EditMessage(ctx, "s3", "t3", "does-not-exist", "nope", false); !errors.Is(err, agent.ErrMessageNotFound) {
		t.Fatalf("expected ErrMessageNotFound for an unknown ID, got %v", err)
	}
}

func TestAgent_RetryMessage_RejectsWrongRoleOrMissing(t *testing.T) {
	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "reply"}}}}
	ag, sessStore, _ := testAgent(t, provider)
	ctx := context.Background()
	if err := ag.RunTurn(ctx, "s4", "t1", "hello"); err != nil {
		t.Fatalf("initial turn failed: %v", err)
	}
	state, err := sessStore.Load("s4")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID := state.History[0].ID

	if err := ag.RetryMessage(ctx, "s4", "t2", userID); !errors.Is(err, agent.ErrMessageNotRetryable) {
		t.Fatalf("expected ErrMessageNotRetryable retrying a user message, got %v", err)
	}
	if err := ag.RetryMessage(ctx, "s4", "t3", "does-not-exist"); !errors.Is(err, agent.ErrMessageNotFound) {
		t.Fatalf("expected ErrMessageNotFound for an unknown ID, got %v", err)
	}
}

// TestAgent_SwitchBranch_UnreachableNestedBranchErrors checks the scope
// decision from the plan: a branch nested behind another branch point is
// only reachable after switching to the outer one first — walking the tree
// one hop at a time — not directly.
func TestAgent_SwitchBranch_UnreachableNestedBranchErrors(t *testing.T) {
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkDelta, Delta: "A"}}, // initial response to "hello"
			{{Kind: agent.StreamChunkDelta, Delta: "B"}}, // retried response
			{{Kind: agent.StreamChunkDelta, Delta: "C"}}, // response after editing the first message
		},
	}
	ag, sessStore, _ := testAgent(t, provider)
	ctx := context.Background()

	if err := ag.RunTurn(ctx, "s5", "t1", "hello"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	state, err := sessStore.Load("s5")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userID := state.History[0].ID
	assistantAID := state.History[1].ID

	// Retry A -> B, stashing A under branches[userID].
	if err := ag.RetryMessage(ctx, "s5", "t2", assistantAID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Edit the *first* message -> C, stashing [hello, B] under branches[""].
	// branches[userID] (holding A) still exists, but userID no longer
	// appears in the active history.
	if err := ag.EditMessage(ctx, "s5", "t3", userID, "hello, edited", false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if err := ag.SwitchBranch("s5", assistantAID); !errors.Is(err, agent.ErrMessageNotFound) {
		t.Fatalf("expected ErrMessageNotFound for a nested-unreachable branch, got %v", err)
	}

	// Switching back to the outer branch makes userID (and so branches[userID]) reachable again.
	if err := ag.SwitchBranch("s5", userID); err != nil {
		t.Fatalf("switch to outer branch: %v", err)
	}
	if err := ag.SwitchBranch("s5", assistantAID); err != nil {
		t.Fatalf("expected the nested branch to now be reachable: %v", err)
	}
}

// TestAgent_LoadSession_MigratesLegacyMessagesWithoutIDs guards a session
// persisted before Message.ID existed: LoadSession must assign IDs in order
// (so they're predictable) and the counter must continue past them, not
// collide with anything this Agent appends from here on.
func TestAgent_LoadSession_MigratesLegacyMessagesWithoutIDs(t *testing.T) {
	provider := &ScriptedProvider{turns: [][]agent.ModelStreamChunk{{{Kind: agent.StreamChunkDelta, Delta: "second response"}}}}
	ag, sessStore, _ := testAgent(t, provider)

	legacy := &agent.SessionState{
		SessionID: "legacy",
		History: []agent.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		},
	}
	if err := sessStore.Save(legacy); err != nil {
		t.Fatalf("saving legacy session: %v", err)
	}

	if err := ag.LoadSession("legacy"); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Migration assigns sequential IDs in order, so the first message is
	// deterministically "m1" — confirmed by editing it directly.
	if err := ag.EditMessage(context.Background(), "legacy", "t1", "m1", "hello, migrated", false); err != nil {
		t.Fatalf("edit of a migrated legacy message failed: %v", err)
	}

	state, err := sessStore.Load("legacy")
	if err != nil {
		t.Fatalf("load after edit: %v", err)
	}
	if state.History[0].Content != "hello, migrated" {
		t.Fatalf("expected the migrated+edited content, got %q", state.History[0].Content)
	}
	// m1, m2 from migration, m3 for the edit, m4 for its regenerated response.
	if state.NextMsgSeq != 4 {
		t.Fatalf("expected NextMsgSeq to continue past the migrated + new IDs, got %d", state.NextMsgSeq)
	}
}

// TestAgent_RetryMessage_NonTerminalRoundTripInMultiToolCallTurn retries the
// *first* assistant message of a turn that went on to run a tool call and a
// second round-trip — not just the last message of a turn. The cut point
// (right after the user message) must still be a valid provider-request
// boundary, and everything after it (the tool call, its result, and the
// final answer) must be discarded together as one stashed alternate.
func TestAgent_RetryMessage_NonTerminalRoundTripInMultiToolCallTurn(t *testing.T) {
	tempDir := t.TempDir()
	norm, err := normalizer.New(tempDir)
	if err != nil {
		t.Fatalf("normalizer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "f.txt"), []byte("contents"), 0644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	engine := permission.New(norm, nil)
	snapStore, err := snapshot.NewDiskSnapshotStore(tempDir)
	if err != nil {
		t.Fatalf("snapshot store: %v", err)
	}
	sessStore, err := agent.NewDiskSessionStore(tempDir)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	readArgs, _ := json.Marshal(map[string]string{"path": "f.txt"})
	provider := &ScriptedProvider{
		turns: [][]agent.ModelStreamChunk{
			{{Kind: agent.StreamChunkToolCall, ToolCall: &agent.ToolCall{ID: "tc1", Name: "file_read", Args: readArgs}}},
			{{Kind: agent.StreamChunkDelta, Delta: "final A"}},
			{{Kind: agent.StreamChunkDelta, Delta: "direct answer"}},
		},
	}

	outBuf := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), outBuf)
	ag := agent.NewAgent(ipc, provider, "test-provider", []tool.Tool{tool.NewFileReadTool(norm)}, engine, snapStore, sessStore, agent.NewContextManager(1000, 100), nil)
	t.Cleanup(ag.WaitBackground)

	ctx := context.Background()
	if err := ag.RunTurn(ctx, "s6", "t1", "read the file"); err != nil {
		t.Fatalf("initial turn: %v", err)
	}

	state, err := sessStore.Load("s6")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 4 {
		t.Fatalf("expected user, assistant(tool call), tool, assistant(final) — got %d messages: %+v", len(state.History), state.History)
	}
	userID := state.History[0].ID
	toolCallAssistantID := state.History[1].ID

	if err := ag.RetryMessage(ctx, "s6", "t2", toolCallAssistantID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	state, err = sessStore.Load("s6")
	if err != nil {
		t.Fatalf("load after retry: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("expected the retry to collapse back to [user, assistant], got %d: %+v", len(state.History), state.History)
	}
	if state.History[1].Content != "direct answer" {
		t.Fatalf("expected the fresh direct answer, got %q", state.History[1].Content)
	}

	alts, ok := state.Branches[userID]
	if !ok || len(alts) != 1 {
		t.Fatalf("expected 1 stashed alternate under the user message, got %+v", state.Branches)
	}
	if len(alts[0]) != 3 || alts[0][0].ID != toolCallAssistantID || alts[0][2].Content != "final A" {
		t.Fatalf("expected the whole 3-message tool round-trip stashed together: %+v", alts[0])
	}
}

// failsOnce errors the first turn (a CLI agent that isn't signed in), then answers.
type failsOnce struct{ calls int }

func (f *failsOnce) StreamTurn(_ context.Context, _ []agent.Message, _ []tool.ToolDefinition, _ string) (<-chan agent.ModelStreamChunk, <-chan error) {
	c, e := make(chan agent.ModelStreamChunk, 2), make(chan error, 1)
	f.calls++
	if f.calls == 1 {
		e <- errors.New("not signed in")
	} else {
		c <- agent.ModelStreamChunk{Kind: agent.StreamChunkDelta, Delta: "reply"}
	}
	close(c)
	close(e)
	return c, e
}

// A turn that failed before any reply was kept leaves the prompt unanswered; the client's retry
// names a bubble the server never had, and must still regenerate rather than say "not found".
func TestAgent_RetryMessage_AnswersPromptWhoseTurnFailed(t *testing.T) {
	ag, sessStore, _ := testAgent(t, &failsOnce{})
	ctx := context.Background()
	if err := ag.RunTurn(ctx, "s6", "t1", "hello"); err == nil {
		t.Fatal("expected the first turn to fail")
	}
	if err := ag.RetryMessage(ctx, "s6", "t2", "t1-reply"); err != nil {
		t.Fatalf("retry of a failed turn: %v", err)
	}
	state, err := sessStore.Load("s6")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(state.History); n != 2 || state.History[1].Content != "reply" {
		t.Fatalf("history = %+v, want the prompt and its reply", state.History)
	}
}
