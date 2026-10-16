package tool

import (
	"sort"
	"yoakeland/internal/protocol"
)

// capturePreMutationSnapshot wraps ExecutionContext.SnapshotStore's
// CapturePreMutation with the snapshot_capturing/snapshot_created/error wire
// events described on ExecutionContext.Stream's doc comment, so file_write
// and file_edit don't duplicate this bracketing logic. A nil SnapshotStore
// or Stream degrades gracefully (no snapshot, or no events, respectively) —
// existing tests that don't wire up a Stream keep working unchanged.
func capturePreMutationSnapshot(execCtx ExecutionContext, description, projectRoot string, absFilePaths ...string) error {
	if execCtx.SnapshotStore == nil {
		return nil
	}

	if execCtx.Stream != nil {
		_ = execCtx.Stream.SendEvent(protocol.EventSnapshotCapturing, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, protocol.SnapshotCapturingData{
			TurnID:      execCtx.TurnID,
			Description: description,
		})
	}

	snap, captured, err := execCtx.SnapshotStore.CapturePreMutation(execCtx.SessionID, execCtx.TurnID, description, projectRoot, absFilePaths...)
	if err != nil {
		if execCtx.Stream != nil {
			_ = execCtx.Stream.SendEvent(protocol.EventError, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, protocol.ErrorData{
				Message: "snapshot capture failed: " + err.Error(),
			})
		}
		return err
	}

	if captured && execCtx.Stream != nil {
		files := make([]string, 0, len(snap.Files))
		for relPath := range snap.Files {
			files = append(files, relPath)
		}
		sort.Strings(files)

		_ = execCtx.Stream.SendEvent(protocol.EventSnapshotCreated, execCtx.SessionID, execCtx.TurnID, execCtx.ToolCallID, protocol.SnapshotEventData{
			SnapshotID:  snap.ID,
			Description: snap.Description,
			Files:       files,
		})
	}

	return nil
}
