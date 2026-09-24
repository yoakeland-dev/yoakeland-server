package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"yoakeland/internal/protocol"
)

type fakeCloser struct {
	closed atomic.Bool
}

func (f *fakeCloser) Close() error {
	f.closed.Store(true)
	return nil
}

// TestRunHeartbeat_PingsWhileAlive guards that an active connection (one
// whose lastSeen keeps advancing) gets periodic pings and is never closed.
func TestRunHeartbeat_PingsWhileAlive(t *testing.T) {
	out := &bytes.Buffer{}
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), out)
	closer := &fakeCloser{}

	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHeartbeat(ctx, ipc, closer, &lastSeen, 10*time.Millisecond, 200*time.Millisecond, nil)
	}()

	// Keep the connection "alive" for a few ping intervals.
	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
		lastSeen.Store(time.Now().UnixNano())
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done // wait for the goroutine to actually exit before reading shared state below

	if closer.closed.Load() {
		t.Fatal("expected an active connection to never be closed by the heartbeat")
	}
	if !strings.Contains(out.String(), `"type":"ping"`) {
		t.Fatalf("expected at least one ping to have been sent, got: %s", out.String())
	}
}

// TestRunHeartbeat_ClosesStaleConnection guards the actual point of the
// heartbeat: a connection that's gone quiet past the timeout gets closed
// instead of held open indefinitely.
func TestRunHeartbeat_ClosesStaleConnection(t *testing.T) {
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &bytes.Buffer{})
	closer := &fakeCloser{}

	var lastSeen atomic.Int64
	// Backdate lastSeen so the very first tick already finds it stale.
	lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHeartbeat(ctx, ipc, closer, &lastSeen, 5*time.Millisecond, 10*time.Millisecond, nil)

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && !closer.closed.Load() {
		time.Sleep(5 * time.Millisecond)
	}

	if !closer.closed.Load() {
		t.Fatal("expected a stale connection to be closed by the heartbeat")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestRunHeartbeat_ClosesOnFailedPing guards that a ping write failure (the
// connection died between traffic, before the timeout would have noticed)
// closes the connection immediately instead of waiting out the full timeout.
func TestRunHeartbeat_ClosesOnFailedPing(t *testing.T) {
	ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), errWriter{})
	closer := &fakeCloser{}

	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHeartbeat(ctx, ipc, closer, &lastSeen, 5*time.Millisecond, time.Hour, nil)

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && !closer.closed.Load() {
		time.Sleep(5 * time.Millisecond)
	}

	if !closer.closed.Load() {
		t.Fatal("expected a failed ping send to close the connection immediately")
	}
}
