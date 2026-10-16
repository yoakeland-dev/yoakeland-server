package terminal

import (
	"bytes"
	"io"
	"testing"
	"yoakeland/internal/protocol"
)

func TestValidateEnv_RejectsLoaderVars(t *testing.T) {
	for _, k := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "PATH", "YOAKELAND_MASTER_KEY"} {
		if _, err := validateEnv(map[string]string{k: "x"}); err == nil {
			t.Errorf("%s accepted", k)
		}
	}
	if out, err := validateEnv(map[string]string{"FOO": "bar"}); err != nil || len(out) != 1 {
		t.Fatalf("plain var rejected: %v %v", out, err)
	}
}

// appendOutput used to send to a subscriber channel after unlocking, so a
// close racing it (tab closed, peer dropped) panicked the daemon.
func TestAppendOutputRacesSubscriberClose(t *testing.T) {
	term := &Terminal{streams: protocol.NewStreamSet()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			term.appendOutput([]byte("x"))
		}
	}()
	for i := 0; i < 2000; i++ {
		s := protocol.NewIPCStream(bytes.NewBuffer(nil), io.Discard)
		term.mu.Lock()
		term.addOutputSubscriberLocked(s)
		term.mu.Unlock()
		if i%2 == 0 {
			term.dropOutputSubscriber(s)
		} else {
			term.closeOutputSubscribers()
		}
	}
	<-done
}
