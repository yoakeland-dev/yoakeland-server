package main

import (
	"bytes"
	"testing"
	"yoakeland/internal/protocol"
)

// TestHandshake guards the server-mode security gate on a TCP connection: it
// must present the correct shared token in an `auth` frame before anything
// else is processed, and a wrong or missing token must be rejected.
func TestHandshake(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"correct token", `{"type":"auth","data":{"token":"secret"}}` + "\n", true},
		{"wrong token", `{"type":"auth","data":{"token":"nope"}}` + "\n", false},
		{"wrong frame type", `{"type":"user_prompt","data":{"prompt":"hi"}}` + "\n", false},
		{"malformed json", `not json` + "\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := bytes.NewBufferString(tc.frame)
			out := &bytes.Buffer{}
			ipc := protocol.NewIPCStream(in, out)

			lookup := func(token string) (string, TokenRecord, bool) {
				if token == "secret" {
					return "default", TokenRecord{Token: "secret"}, true
				}
				return "", TokenRecord{}, false
			}
			_, _, got := handshake(ipc, "tcp", lookup)
			if got != tc.want {
				t.Fatalf("handshake() ok = %v, want %v (out: %s)", got, tc.want, out.String())
			}
		})
	}
}

// TestHandshake_UnixAllowsOptionalAuth guards the local-socket contract from
// SPEC.md §2a: an anonymous first envelope is handed back as pending rather
// than rejected, and an explicit `auth` frame (no token needed) is consumed
// to capture Name/DeviceType instead.
func TestHandshake_UnixAllowsOptionalAuth(t *testing.T) {
	t.Run("no auth frame", func(t *testing.T) {
		in := bytes.NewBufferString(`{"type":"ping","data":{}}` + "\n")
		ipc := protocol.NewIPCStream(in, &bytes.Buffer{})

		who, pending, ok := handshake(ipc, "unix", nil)
		name, deviceType := who.Name, who.DeviceType
		if !ok || pending == nil || pending.Type != protocol.EventPing {
			t.Fatalf("handshake() = name=%q deviceType=%q pending=%v ok=%v, want the ping envelope handed back pending", name, deviceType, pending, ok)
		}
	})

	t.Run("self-reported identity, no token required", func(t *testing.T) {
		in := bytes.NewBufferString(`{"type":"auth","data":{"name":"aveline-laptop","deviceType":"desktop","deviceId":"dev-1"}}` + "\n")
		ipc := protocol.NewIPCStream(in, &bytes.Buffer{})

		who, pending, ok := handshake(ipc, "unix", nil)
		name, deviceType := who.Name, who.DeviceType
		if !ok || pending != nil || name != "aveline-laptop" || deviceType != "desktop" || who.DeviceID != "dev-1" {
			t.Fatalf("handshake() = name=%q deviceType=%q pending=%v ok=%v, want identity captured with no pending envelope", name, deviceType, pending, ok)
		}
	})
}
