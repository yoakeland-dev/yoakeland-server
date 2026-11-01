package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yoakeland/internal/protocol"
)

func TestACPRegistryAndPairInfoNeedFullAccess(t *testing.T) {
	deps := newTestDeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agents":[{"id":"a","name":"A","version":"1","description":"d","distribution":{"npx":{"package":"p"},"binary":{"x":1}}}]}`))
	}))
	defer srv.Close()
	oldURL := acpRegistryURL
	acpRegistryURL = srv.URL
	acpCache.at = time.Time{}
	t.Cleanup(func() { acpRegistryURL = oldURL; acpCache.at, acpCache.fail = time.Time{}, nil })

	for _, c := range []struct {
		name       string
		restricted bool
		want       bool
	}{{"full access", false, true}, {"paired phone", true, false}} {
		var out syncBuf
		ipc := protocol.NewIPCStream(bytes.NewBuffer(nil), &out)
		p := deps.peers.Add(ipc, &fakeCloser{}, "tcp", peerIdentity{Name: "web", DeviceType: "web", Restricted: c.restricted})
		for _, typ := range []protocol.EventType{protocol.EventListACPRegistry, protocol.EventGetPairInfo} {
			handleEnvelope(context.Background(), ipc, deps, "tcp", p.ID, &protocol.Envelope{Type: typ, Data: json.RawMessage(`{"requestId":"r1"}`)})
		}
		for i := 0; i < 100 && c.want && !strings.Contains(out.String(), `"acp_registry"`); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		got := out.String()
		if c.want {
			if !strings.Contains(got, `"acp_registry"`) || !strings.Contains(got, `"package":"p"`) || strings.Contains(got, `"binary"`) || !strings.Contains(got, `"pair_info"`) || !strings.Contains(got, `"requestId":"r1"`) {
				t.Errorf("%s: got %s", c.name, got)
			}
		} else if strings.Contains(got, `"acp_registry"`) || strings.Contains(got, `"pair_info"`) || strings.Count(got, "isn't available from a paired device") != 2 {
			t.Errorf("%s: got %s", c.name, got)
		}
	}
}
