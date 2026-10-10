package main

import (
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

// restart_server is an admin action: only the local socket may ask, and asking stops the daemon so
// that whatever started it can start it again.
func TestRestartServer(t *testing.T) {
	deps := newTestDeps(t)
	stopped := make(chan struct{})
	shutdownServer = func() { close(stopped) }
	t.Cleanup(func() { shutdownServer = nil })
	line := envelopeJSON(t, protocol.EventRestartServer, protocol.RestartServerData{})

	envs := runDispatchNetwork(t, deps, "tcp", line)
	if len(envs) != 1 || envs[0].Type != protocol.EventError {
		t.Fatalf("restart over tcp: %+v, want an error", envs)
	}
	select {
	case <-stopped:
		t.Fatal("a remote peer stopped the server")
	case <-time.After(500 * time.Millisecond):
	}

	envs = runDispatchNetwork(t, deps, "unix", line)
	if len(envs) == 0 || envs[len(envs)-1].Type != protocol.EventServerRestart {
		t.Fatalf("restart over unix: %+v, want server_restart", envs)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the server was never told to stop")
	}
}
