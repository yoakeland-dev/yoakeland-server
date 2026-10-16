//go:build unix

package snapshot_test

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"yoakeland/internal/snapshot"
)

// A FIFO at a path a turn is about to write must be refused, not waited on:
// a read would block until a writer appears, with the store's lock held.
func TestDiskSnapshotStore_CaptureRefusesFIFO(t *testing.T) {
	root := t.TempDir()
	store, err := snapshot.NewDiskSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := store.CapturePreMutation("s1", "t1", "write", root, fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a refusal for a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture blocked on a FIFO")
	}
}
