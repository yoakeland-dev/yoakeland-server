//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

func untilTrue(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A chat that has sent nothing yet can still have a terminal (the project in terminal_open is all
// the server needs), and the shell goes once its only viewer detaches or disconnects.
func TestTerminalLifecycle(t *testing.T) {
	for _, how := range []string{"detach", "disconnect"} {
		t.Run(how, func(t *testing.T) {
			deps := newTestDeps(t)
			pc, err := deps.projects.Get(deps.defaultProjectRoot)
			if err != nil {
				t.Fatal(err)
			}
			inR, inW := io.Pipe()
			out := &lockedBuf{}
			done := make(chan struct{})
			go func() {
				serveConnection(context.Background(), protocol.NewIPCStream(inR, out), deps, "unix", "", nil)
				close(done)
			}()
			send := func(line string) {
				if _, err := io.WriteString(inW, line+"\n"); err != nil {
					t.Fatal(err)
				}
			}

			sess := "draft-" + how
			send(fmt.Sprintf(`{"type":"terminal_open","sessionId":%q,"data":{"sessionId":%q,"projectPath":%q,"terminalId":"t1","cols":80,"rows":24}}`, sess, sess, deps.defaultProjectRoot))
			untilTrue(t, "a snapshot", func() bool { return strings.Contains(out.String(), `"terminal_snapshot"`) })
			if !strings.Contains(out.String(), `"sandboxTier":"host"`) {
				t.Fatalf("a person's terminal wasn't a host shell: %s", out.String())
			}
			if n := len(pc.Terminals.All()); n != 1 {
				t.Fatalf("%d terminals after open, want 1", n)
			}

			if how == "detach" {
				send(fmt.Sprintf(`{"type":"terminal_detach","sessionId":%q,"data":{"sessionId":%q,"projectPath":%q,"terminalId":"t1"}}`, sess, sess, deps.defaultProjectRoot))
			} else {
				inW.Close()
				<-done
			}
			untilTrue(t, "the idle shell to be forgotten", func() bool { return len(pc.Terminals.All()) == 0 })
			inW.Close()
		})
	}
}
