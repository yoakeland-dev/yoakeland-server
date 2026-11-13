package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yoakeland/internal/agent"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/sched"
	"yoakeland/internal/tool"
)

// pipeW is the write side of a test connection: every event the server sends it goes to fn, on the
// sender's goroutine, which is how a test device stands between two servers.
type pipeW struct {
	mu sync.Mutex
	fn func(protocol.Envelope)
}

func (w *pipeW) set(fn func(protocol.Envelope)) { w.mu.Lock(); w.fn = fn; w.mu.Unlock() }

func (w *pipeW) Write(b []byte) (int, error) {
	w.mu.Lock()
	fn := w.fn
	w.mu.Unlock()
	var env protocol.Envelope
	if fn != nil && json.Unmarshal(bytes.TrimSpace(b), &env) == nil {
		fn(env)
	}
	return len(b), nil
}

func addTestPeer(t *testing.T, deps connDeps, network string) (*Peer, *pipeW) {
	t.Helper()
	w := &pipeW{}
	p := deps.peers.Add(protocol.NewIPCStream(strings.NewReader(""), w), nopCloser{}, network, peerIdentity{Name: "dev", DeviceType: "desktop"})
	t.Cleanup(func() { deps.peers.Remove(p.ID) })
	return p, w
}

// fakeModel is an OpenAI-compatible endpoint standing in for A's provider.
type fakeModel struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	hits    atomic.Int32
	gone    chan struct{} // one send per request whose client went away
	handler func(n int, w http.ResponseWriter, r *http.Request)
}

func sse(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func newFakeModel(t *testing.T) *fakeModel {
	m := &fakeModel{gone: make(chan struct{}, 8)}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(m.hits.Add(1))
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if m.handler != nil {
			m.handler(n, w, r)
			return
		}
		m.answer(w)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeModel) answer(w http.ResponseWriter) {
	sse(w, `{"choices":[{"delta":{"content":"he"}}]}`)
	sse(w, `{"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`)
	sse(w, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	sse(w, `[DONE]`)
}

// hold sends one delta, then waits until released or the client goes away.
func (m *fakeModel) hold(release <-chan struct{}) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"content":"he"}}]}`)
		select {
		case <-release:
			sse(w, `{"choices":[{"delta":{"content":"llo"}}]}`)
			sse(w, `[DONE]`)
		case <-r.Context().Done():
			m.gone <- struct{}{}
		}
	}
}

// bKey is what the test device verified about its connection to B.
const bKey = "ca:b"

type fwdWorld struct {
	a, b   connDeps
	model  *fakeModel
	aID    string
	bID    string
	states chan protocol.ForwardStateData
}

func newFwdWorld(t *testing.T) *fwdWorld {
	t.Helper()
	w := &fwdWorld{a: newTestDeps(t), b: newTestDeps(t), model: newFakeModel(t), states: make(chan protocol.ForwardStateData, 16)}
	w.a.forward, w.b.forward = newForwarder(), newForwarder()
	w.aID, w.bID = w.a.settings.GetServerID(), w.b.settings.GetServerID()
	// The owner lent p to B, as they would from B's model picker.
	if err := w.a.settings.GrantForward(ownerID, w.bID, bKey, "B", "p"); err != nil {
		t.Fatal(err)
	}
	w.b.projects.rememberOwner("chat1", ownerID)
	if err := w.a.providerStore.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", BaseURL: w.model.srv.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	_, watch := addTestPeer(t, w.b, "unix")
	watch.set(func(env protocol.Envelope) {
		if env.Type == protocol.EventForwardState {
			var d protocol.ForwardStateData
			_ = json.Unmarshal(env.Data, &d)
			w.states <- d
		}
	})
	return w
}

type fwdDevice struct {
	w            *fwdWorld
	aPeer, bPeer *Peer
	aw           *pipeW
}

// newDevice connects one device to both servers and has it pass frames between them untouched.
func (w *fwdWorld) newDevice(t *testing.T) *fwdDevice {
	d := &fwdDevice{w: w}
	var bw *pipeW
	var aw *pipeW
	d.aPeer, aw = addTestPeer(t, w.a, "unix")
	d.aw = aw
	d.bPeer, bw = addTestPeer(t, w.b, "unix")
	ctx := context.Background()
	bw.set(func(env protocol.Envelope) { // B to A
		if env.Type == protocol.EventForwardRequest { // the device says where it came from, whatever B wrote
			var rd protocol.ForwardRequestData
			_ = json.Unmarshal(env.Data, &rd)
			rd.From, rd.FromKey = w.bID, bKey
			env.Data, _ = json.Marshal(rd)
		}
		if env.Type == protocol.EventForwardRequest || env.Type == protocol.EventForwardCancel {
			handleForwardEnvelope(ctx, d.aPeer.ipc, w.a, "unix", d.aPeer.ID, &env)
		}
	})
	aw.set(func(env protocol.Envelope) { // A to B
		if env.Type == protocol.EventForwardChunk || env.Type == protocol.EventForwardEnd {
			handleForwardEnvelope(ctx, d.bPeer.ipc, w.b, "unix", d.bPeer.ID, &env)
		}
	})
	return d
}

func (d *fwdDevice) offer(servers ...string) {
	raw, _ := json.Marshal(protocol.RelayOfferData{Servers: servers})
	handleForwardEnvelope(context.Background(), d.bPeer.ipc, d.w.b, "unix", d.bPeer.ID, &protocol.Envelope{Type: protocol.EventRelayOffer, Data: raw})
}

// lose is the device's connections ending, as serveConnection's exit does it.
func (d *fwdDevice) lose() {
	d.aw.set(nil) // a dead device carries nothing more from A, not even A's "cancelled"
	d.w.a.forward.connClosed(d.w.a, d.aPeer.ipc, d.aPeer.ID)
	d.w.b.forward.connClosed(d.w.b, d.bPeer.ipc, d.bPeer.ID)
}

func (w *fwdWorld) provider(t *testing.T) *ForwardedProvider {
	p, err := newForwardedProvider(w.b, "chat1", w.aID, "p", "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(t *testing.T, chunks <-chan agent.ModelStreamChunk, errs <-chan error) (text string, usage int, err error) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for chunks != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				break
			}
			switch c.Kind {
			case agent.StreamChunkDelta:
				text += c.Delta
			case agent.StreamChunkUsage:
				usage += c.Usage.TotalTokens
			}
		case <-timeout:
			t.Fatal("stream didn't finish")
		}
	}
	return text, usage, <-errs
}

// settled waits for forwarding to hold nothing on either server: no goroutine's entry may outlive its request.
func (w *fwdWorld) settled(t *testing.T) {
	t.Helper()
	for i := 0; i < 200; i++ {
		w.a.forward.mu.Lock()
		w.b.forward.mu.Lock()
		n := len(w.a.forward.conns) + len(w.b.forward.calls)
		w.b.forward.mu.Unlock()
		w.a.forward.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("forwarding still holds a request")
}

func TestForwardEndToEnd(t *testing.T) {
	w := newFwdWorld(t)
	w.newDevice(t).offer(w.aID)
	p := w.provider(t)
	p.SetModelOptions(agent.ModelOptions{ReasoningEffort: "low"})
	// More than one piece, with multi-byte characters at every offset.
	system := strings.Repeat("é日", protocol.ForwardPieceSize)
	chunks, errs := p.StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, system)
	text, usage, err := collect(t, chunks, errs)
	if err != nil || text != "hello" || usage != 10 {
		t.Fatalf("got %q usage %d err %v", text, usage, err)
	}
	body := w.model.bodies[0]
	msgs := body["messages"].([]any)
	if body["model"] != "gpt-x" || msgs[0].(map[string]any)["content"] != system || body["reasoning_effort"] != "low" {
		t.Fatalf("A's provider got a different request: model %v, effort %v, system intact %v", body["model"], body["reasoning_effort"], msgs[0].(map[string]any)["content"] == system)
	}
	w.settled(t)
}

func TestForwardTitle(t *testing.T) {
	w := newFwdWorld(t)
	w.newDevice(t).offer(w.aID)
	title, err := w.provider(t).GenerateTitle(context.Background(), "hello there")
	if err != nil || title != "hello" {
		t.Fatalf("title %q err %v", title, err)
	}
	w.settled(t)
}

func TestForwardWireRoundTrip(t *testing.T) {
	yes := true
	msg := agent.Message{
		ID: "m1", Role: "assistant", Content: "c", Reasoning: "r", ToolCallID: "tc",
		ToolCalls: []agent.ToolCall{{ID: "t1", Name: "bash", Args: json.RawMessage(`{"cmd":"ls"}`), NativeToolInfo: protocol.NativeToolInfo{Native: true, Title: "x", Kind: "execute", Locations: []string{"/a"}}}},
		Images:    []protocol.ImageData{{MimeType: "image/png", Data: "AAAA", Width: 2, Height: 3}},
		Subagent:  []agent.Message{{Role: "user", Content: "inner"}}, Summary: "s", Ask: "a",
		Actions: []protocol.QuickAction{{Label: "l", Prompt: "p"}}, AuthorName: "n", AuthorID: "u",
		Attachments: []string{"/x"}, Provider: "p", Model: "m", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), GenerationMs: 12,
		Usage:      &protocol.UsageData{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CostUSD: 0.5, CostKnown: true, CachedInputTokens: 4, ContextTokens: 5},
		RawContent: json.RawMessage(`[{"type":"thinking","signature":"sig"}]`), PrefixKey: "pk",
		Compaction: &agent.Compaction{Kind: "summary", TokensBefore: 9, TokensAfter: 3},
	}
	in := forwardBody{
		ProviderName: "p", Model: "m", Messages: []agent.Message{msg}, SystemPrompt: "sys",
		Tools:   []tool.ToolDefinition{{Name: "t", Description: "d", Parameters: map[string]any{"type": "object", "required": []any{"a"}}}},
		Hints:   agent.RequestHints{CacheKey: "k", AdvisorModel: "a", ServerWebSearch: true},
		Options: agent.ModelOptions{ReasoningEffort: "high", ThinkingBudget: 5, FastMode: &yes, AutoCompact: &yes},
	}
	raw, _ := json.Marshal(in)
	var out forwardBody
	// Tiny pieces: every cut lands somewhere.
	if err := json.Unmarshal([]byte(strings.Join(splitPieces(raw, 7), "")), &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("body changed on the way:\n in %+v\nout %+v", in, out)
	}

	for _, c := range []agent.ModelStreamChunk{
		{Kind: agent.StreamChunkDelta, Delta: "x"},
		{Kind: agent.StreamChunkReasoning, Delta: "why"},
		{Kind: agent.StreamChunkToolCall, ToolCall: &msg.ToolCalls[0]},
		{Kind: agent.StreamChunkUsage, Usage: in.Messages[0].Usage},
		{Kind: agent.StreamChunkFinish, FinishReason: "length"},
		{Kind: agent.StreamChunkRawContent, Raw: json.RawMessage(`[{"a":1}]`)},
		{Kind: agent.StreamChunkContext, Context: &agent.ContextSnapshot{ContextTokens: 1, WindowTokens: 2, Source: "provider"}},
	} {
		w, ok := toWireChunk(c)
		raw, _ := json.Marshal(w)
		var back protocol.ForwardChunk
		if err := json.Unmarshal(raw, &back); err != nil || !ok {
			t.Fatalf("%s: %v ok=%v", c.Kind, err, ok)
		}
		if got := fromWireChunk(back); !reflect.DeepEqual(got, c) {
			t.Errorf("%s changed: %+v != %+v", c.Kind, got, c)
		}
	}
}

// capture records what a server wrote to a connection.
type capture struct {
	mu   sync.Mutex
	envs []protocol.Envelope
}

func (c *capture) Write(b []byte) (int, error) {
	var env protocol.Envelope
	if json.Unmarshal(bytes.TrimSpace(b), &env) == nil {
		c.mu.Lock()
		c.envs = append(c.envs, env)
		c.mu.Unlock()
	}
	return len(b), nil
}

func (c *capture) end(t *testing.T, id string) string {
	t.Helper()
	for i := 0; i < 500; i++ {
		c.mu.Lock()
		for _, e := range c.envs {
			var d protocol.ForwardEndData
			if e.Type == protocol.EventForwardEnd && json.Unmarshal(e.Data, &d) == nil && d.RequestID == id {
				c.mu.Unlock()
				return d.Error
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no forward_end for %s", id)
	return ""
}

func TestForwardRefusals(t *testing.T) {
	w := newFwdWorld(t)
	if err := w.a.providerStore.Register(provider.ProviderConfig{Name: "cli", Kind: provider.ProviderKindCLIClaude}); err != nil {
		t.Fatal(err)
	}
	if err := w.a.providerStore.Register(provider.ProviderConfig{Name: "codex", Kind: provider.ProviderKindCLICodex}); err != nil {
		t.Fatal(err)
	}
	// Lent by hand: the grant event itself refuses codex.
	if err := w.a.settings.GrantForward(ownerID, w.bID, bKey, "B", "codex"); err != nil {
		t.Fatal(err)
	}
	body := func(name string) string {
		raw, _ := json.Marshal(forwardBody{ProviderName: name, Model: "m", Messages: []agent.Message{{Role: "user", Content: "hi"}}})
		return string(raw)
	}
	piece := func(id, server string, seq int, last bool, data string) *protocol.Envelope {
		raw, _ := json.Marshal(protocol.ForwardRequestData{RequestID: id, ServerID: server, From: w.bID, FromKey: bKey, Seq: seq, Last: last, Data: data})
		return &protocol.Envelope{Type: protocol.EventForwardRequest, Data: raw}
	}
	conn := func(network string) (*Peer, *capture) {
		c := &capture{}
		p := w.a.peers.Add(protocol.NewIPCStream(strings.NewReader(""), c), nopCloser{}, network, peerIdentity{Name: "x"})
		t.Cleanup(func() { w.a.forward.connClosed(w.a, p.ipc, p.ID); w.a.peers.Remove(p.ID) })
		return p, c
	}
	for _, tc := range []struct {
		name, network, want string
		env                 func() []*protocol.Envelope
	}{
		{"wrong server", "unix", "this server is not other", func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", "other", 0, true, body("p"))}
		}},
		{"too big", "unix", "over 64 MiB", func() []*protocol.Envelope {
			big := strings.Repeat("a", 33<<20)
			return []*protocol.Envelope{piece("r", w.aID, 0, false, big), piece("r", w.aID, 1, false, big)}
		}},
		{"out of order", "unix", "out of order", func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", w.aID, 1, true, body("p"))}
		}},
		{"unknown provider", "unix", notLent, func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", w.aID, 0, true, body("nope"))}
		}},
		{"nothing lent", "tcp", notLent, func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", w.aID, 0, true, body("cli"))}
		}},
		{"cli kind", "unix", notLent, func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", w.aID, 0, true, body("cli"))}
		}},
		{"codex kind", "unix", "can't be used from another server", func() []*protocol.Envelope {
			return []*protocol.Envelope{piece("r", w.aID, 0, true, body("codex"))}
		}},
		{"too many in flight", "unix", "in flight", func() []*protocol.Envelope {
			var out []*protocol.Envelope
			for i := 0; i <= maxForwardInFlight; i++ {
				out = append(out, piece(fmt.Sprint("r", i), w.aID, 0, false, "{"))
			}
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c := conn(tc.network)
			envs := tc.env()
			for _, e := range envs {
				handleForwardEnvelope(context.Background(), p.ipc, w.a, tc.network, p.ID, e)
			}
			var d protocol.ForwardRequestData
			_ = json.Unmarshal(envs[len(envs)-1].Data, &d)
			if got := c.end(t, d.RequestID); !strings.Contains(got, tc.want) {
				t.Fatalf("refusal %q, want it to mention %q", got, tc.want)
			}
		})
	}
	if w.model.hits.Load() != 0 {
		t.Fatal("a refused request reached the provider")
	}
}

func TestForwardCancelFromB(t *testing.T) {
	w := newFwdWorld(t)
	w.model.handler = w.model.hold(nil)
	w.newDevice(t).offer(w.aID)
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := w.provider(t).StreamTurn(ctx, []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
	if c := <-chunks; c.Delta != "he" {
		t.Fatalf("first chunk %+v", c)
	}
	cancel()
	_, _, err := collect(t, chunks, errs)
	if err == nil || err != context.Canceled {
		t.Fatalf("err = %v, want cancelled", err)
	}
	select {
	case <-w.model.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("A's provider kept running after forward_cancel")
	}
	w.settled(t)
}

func TestForwardAConnectionClosing(t *testing.T) {
	w := newFwdWorld(t)
	w.model.handler = w.model.hold(nil)
	d := w.newDevice(t)
	d.offer(w.aID)
	chunks, errs := w.provider(t).StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
	<-chunks
	w.a.forward.connClosed(w.a, d.aPeer.ipc, d.aPeer.ID)
	_, _, err := collect(t, chunks, errs)
	if err == nil {
		t.Fatal("a request whose connection closed on A finished as an answer")
	}
	select {
	case <-w.model.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("A's provider kept running after its connection closed")
	}
	w.settled(t)
}

func TestForwardWaitsForADevice(t *testing.T) {
	w := newFwdWorld(t)
	chunks, errs := w.provider(t).StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
	if s := <-w.states; s.State != protocol.ForwardWaiting || s.ServerID != w.aID {
		t.Fatalf("state %+v", s)
	}
	// A device that offers some other server doesn't end the wait.
	d := w.newDevice(t)
	d.offer("elsewhere")
	select {
	case s := <-w.states:
		t.Fatalf("unexpected state %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
	d.offer(w.aID)
	text, _, err := collect(t, chunks, errs)
	if err != nil || text != "hello" {
		t.Fatalf("got %q err %v", text, err)
	}
	if s := <-w.states; s.State != protocol.ForwardRelaying {
		t.Fatalf("state %+v", s)
	}
	w.settled(t)
}

func TestForwardGivesUpWaiting(t *testing.T) {
	old := forwardWaitLimit
	forwardWaitLimit = 50 * time.Millisecond
	defer func() { forwardWaitLimit = old }()
	w := newFwdWorld(t)
	chunks, errs := w.provider(t).StreamTurn(context.Background(), nil, nil, "")
	_, _, err := collect(t, chunks, errs)
	if err == nil || !strings.Contains(err.Error(), "no device connected to both this server and the one that has this provider came online") {
		t.Fatalf("err = %v", err)
	}
}

func TestForwardCancelWhileWaiting(t *testing.T) {
	w := newFwdWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := w.provider(t).StreamTurn(ctx, nil, nil, "")
	<-w.states
	cancel()
	for range chunks {
	}
	if err := <-errs; err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
}

func TestForwardDeviceLostBeforeAndAfterFirstChunk(t *testing.T) {
	t.Run("before: another device takes over", func(t *testing.T) {
		w := newFwdWorld(t)
		w.model.handler = func(n int, rw http.ResponseWriter, r *http.Request) {
			if n == 1 { // the first request never answers
				<-r.Context().Done()
				w.model.gone <- struct{}{}
				return
			}
			w.model.answer(rw)
		}
		first := w.newDevice(t)
		first.offer(w.aID)
		chunks, errs := w.provider(t).StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
		for w.model.hits.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		first.lose()
		<-w.states // waiting: nobody else is connected yet
		w.newDevice(t).offer(w.aID)
		text, _, err := collect(t, chunks, errs)
		if err != nil || text != "hello" {
			t.Fatalf("got %q err %v", text, err)
		}
		w.settled(t)
	})
	t.Run("after: the request fails and is not replayed", func(t *testing.T) {
		w := newFwdWorld(t)
		w.model.handler = w.model.hold(nil)
		d := w.newDevice(t)
		d.offer(w.aID)
		chunks, errs := w.provider(t).StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
		<-chunks
		d.lose()
		w.newDevice(t).offer(w.aID) // a replacement must not be given a half-streamed answer to redo
		_, _, err := collect(t, chunks, errs)
		want := "the device relaying to the server that has this provider disconnected; send a message to continue"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if w.model.hits.Load() != 1 {
			t.Fatalf("the request was replayed (%d hits)", w.model.hits.Load())
		}
		w.settled(t)
	})
}

func TestForwardIgnoresRepliesFromOtherDevices(t *testing.T) {
	w := newFwdWorld(t)
	f := w.b.forward
	call := f.addCall("req", "dev1")
	defer f.removeCall("req", call)
	f.deliver("dev2", "req", fwdMsg{end: &protocol.ForwardEndData{RequestID: "req"}})
	f.deliver("dev1", "unknown", fwdMsg{end: &protocol.ForwardEndData{RequestID: "unknown"}})
	if len(call.msgs) != 0 {
		t.Fatal("a reply from a device that wasn't given the request was accepted")
	}
	f.deliver("dev1", "req", fwdMsg{end: &protocol.ForwardEndData{RequestID: "req"}})
	if len(call.msgs) != 1 {
		t.Fatal("the reply from the right device was dropped")
	}
}

// The request runs on A as whoever the device is there, so only the chat owner's own devices relay
// it: a Full person who may read a Limited person's chat mustn't spend their grant on it.
func TestForwardOnlyTheChatOwnersDevices(t *testing.T) {
	w := newFwdWorld(t)
	limited, full := addPerson(t, w.b, TierLimited), addPerson(t, w.b, TierFull)
	w.b.projects.rememberOwner("theirs", limited.UserID)
	offer := func(p *Peer) {
		raw, _ := json.Marshal(protocol.RelayOfferData{Servers: []string{w.aID}})
		handleForwardEnvelope(context.Background(), p.ipc, w.b, "tcp", p.ID, &protocol.Envelope{Type: protocol.EventRelayOffer, Data: raw})
	}
	offer(full)
	if p := w.b.forward.pick(w.b, "theirs", w.aID, full.ID); p != nil {
		t.Fatal("someone else's device relayed the chat")
	}
	if p := w.b.forward.pick(w.b, "unknown-chat", w.aID, ""); p != nil {
		t.Fatal("a chat with no known owner was relayed")
	}
	offer(limited)
	if p := w.b.forward.pick(w.b, "theirs", w.aID, full.ID); p == nil || p.ID != limited.ID {
		t.Fatal("the owner's device wasn't picked")
	}
}

func TestForwardPrefersTheDeviceThatStartedTheTurn(t *testing.T) {
	w := newFwdWorld(t)
	first, second := w.newDevice(t), w.newDevice(t)
	first.offer(w.aID)
	second.offer(w.aID)
	if p := w.b.forward.pick(w.b, "chat1", w.aID, first.bPeer.ID); p.ID != first.bPeer.ID {
		t.Fatal("the preferred device wasn't picked")
	}
}

func TestForwardViaBindingSurvivesRestoreBinding(t *testing.T) {
	deps := newTestDeps(t)
	deps.forward = newForwarder()
	deps.sched = sched.New(t.TempDir())
	pc, err := deps.projects.Resolve("s1", "", deps.defaultProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	ipc := protocol.NewIPCStream(strings.NewReader(""), io.Discard)
	ag := pc.Registry.Get(ipc, "s1")
	name := "via:srv-elsewhere:my:key"
	if err := bindProvider(deps, pc, ag, "s1", name, "gpt-x"); err != nil {
		t.Fatal(err)
	}
	if b, _ := deps.sched.BindingFor("s1"); ag.ProviderName() != name || b.Provider != name || b.Model != "gpt-x" || !ag.SetModelOptions(agent.ModelOptions{}, 0, nil, nil) {
		t.Fatalf("bound %q, remembered %+v", ag.ProviderName(), b)
	}
	// The server restarts: the schedule is read back and the chat has no provider yet.
	deps.sched = sched.New(deps.sched.Dir())
	ag.SetProvider(nil, "")
	restoreBinding(deps, pc, ag, "s1")
	if ag.ProviderName() != name || !ag.SetModelOptions(agent.ModelOptions{}, 0, nil, nil) {
		t.Fatalf("restored %q", ag.ProviderName())
	}
	// Your own server's provider is used directly, not through a device.
	if err := bindProvider(deps, pc, ag, "s1", "via:"+deps.settings.GetServerID()+":p", "m"); err == nil {
		t.Fatal("bound a via: provider that lives on this server")
	}
	if !providerAllowed(deps, Access{Tier: TierLimited}, name) {
		t.Fatal("a limited person can't pick a borrowed provider")
	}
}

func TestForwardingWireNames(t *testing.T) {
	deps := newTestDeps(t)
	deps.providerStore.Register(provider.ProviderConfig{Name: "k", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", APIKey: "secret"})
	envs := runDispatch(t, deps, envelopeJSON(t, protocol.EventListProviders, struct{}{}))
	var got protocol.ProviderListData
	var listed string
	for _, e := range envs {
		if e.Type == protocol.EventProviderList {
			_ = json.Unmarshal(e.Data, &got)
			listed = string(e.Data)
		}
	}
	cfg, _ := deps.providerStore.Get("k")
	if len(got.Providers) != 1 || got.Providers[0].KeyID != cfg.KeyID() || len(cfg.KeyID()) != 16 || strings.Contains(listed, "secret") {
		t.Fatalf("providers %+v, own keyId %q, listed %s", got, cfg.KeyID(), listed)
	}
	if err := registerProviderRefusal(deps, Access{Tier: TierFull}, provider.ProviderConfig{Name: "via:x:y", Kind: provider.ProviderKindAPIKey}); err == "" {
		t.Fatal("registered a provider named like a borrowed one")
	}
	var id protocol.ServerIdentityData
	p, w := addTestPeer(t, deps, "unix")
	w.set(func(e protocol.Envelope) {
		if e.Type == protocol.EventServerIdentity {
			_ = json.Unmarshal(e.Data, &id)
		}
	})
	sendServerIdentity(deps, p)
	if !reflect.DeepEqual(id.Features, []string{"forwarding"}) {
		t.Fatalf("features %v", id.Features)
	}
}

// request sends one whole forward_request to A from a connection and returns A's forward_end error text.
func (w *fwdWorld) request(t *testing.T, from, provider string) (errText string, hits int32) {
	t.Helper()
	return w.requestKeyed(t, from, bKey, provider)
}

func (w *fwdWorld) requestKeyed(t *testing.T, from, fromKey, provider string) (errText string, hits int32) {
	t.Helper()
	c := &capture{}
	p := w.a.peers.Add(protocol.NewIPCStream(strings.NewReader(""), c), nopCloser{}, "unix", peerIdentity{Name: "x"})
	defer func() { w.a.forward.connClosed(w.a, p.ipc, p.ID); w.a.peers.Remove(p.ID) }()
	raw, _ := json.Marshal(forwardBody{ProviderName: provider, Model: "m", Messages: []agent.Message{{Role: "user", Content: "hi"}}})
	d, _ := json.Marshal(protocol.ForwardRequestData{RequestID: "r", ServerID: w.aID, From: from, FromKey: fromKey, Last: true, Data: string(raw)})
	before := w.model.hits.Load()
	handleForwardEnvelope(context.Background(), p.ipc, w.a, "unix", p.ID, &protocol.Envelope{Type: protocol.EventForwardRequest, Data: d})
	e := c.end(t, "r")
	return e, w.model.hits.Load() - before
}

func TestForwardNeedsAGrant(t *testing.T) {
	w := newFwdWorld(t)
	refused := notLent
	if e, hits := w.request(t, w.bID, "p"); e != "" || hits != 1 {
		t.Fatalf("granted request: err %q hits %d", e, hits)
	}
	// A server that claims B's id isn't B: the device reached it by something else.
	if e, hits := w.requestKeyed(t, w.bID, "ca:impostor", "p"); e != refused || hits != 0 {
		t.Fatalf("B's id from another server: err %q hits %d", e, hits)
	}
	if e, hits := w.requestKeyed(t, w.bID, "", "p"); e != refused || hits != 0 {
		t.Fatalf("no key: err %q hits %d", e, hits)
	}
	// Whether a provider exists is no business of a server it isn't lent to.
	if e, _ := w.request(t, w.bID, "nope"); e != refused {
		t.Fatalf("unknown provider: %q", e)
	}
	if e, hits := w.request(t, "", "p"); e != refused || hits != 0 {
		t.Fatalf("no from: err %q hits %d", e, hits)
	}
	if e, hits := w.request(t, "some-other-server", "p"); e != refused || hits != 0 {
		t.Fatalf("grant for another server: err %q hits %d", e, hits)
	}
	if err := w.a.providerStore.Register(provider.ProviderConfig{Name: "q", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", BaseURL: w.model.srv.URL, APIKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := w.request(t, w.bID, "q"); e != refused {
		t.Fatalf("another provider of the same person: %q", e)
	}
	if err := w.a.settings.RevokeForward(ownerID, w.bID, "p"); err != nil {
		t.Fatal(err)
	}
	if e, hits := w.request(t, w.bID, "p"); e != refused || hits != 0 {
		t.Fatalf("after revoke: err %q hits %d", e, hits)
	}
}

// grants runs one grant event as the sender and returns the forward_grants it was answered with.
func grants(t *testing.T, deps connDeps, network, peerID string, typ protocol.EventType, data any) (protocol.ForwardGrantsData, string) {
	t.Helper()
	c := &capture{}
	ipc := protocol.NewIPCStream(strings.NewReader(""), c)
	raw, _ := json.Marshal(data)
	if !handleForwardEnvelope(context.Background(), ipc, deps, network, peerID, &protocol.Envelope{Type: typ, Data: raw}) {
		t.Fatalf("%s not handled", typ)
	}
	var out protocol.ForwardGrantsData
	var errText string
	for _, e := range c.envs {
		switch e.Type {
		case protocol.EventForwardGrants:
			_ = json.Unmarshal(e.Data, &out)
		case protocol.EventError:
			var d protocol.ErrorData
			_ = json.Unmarshal(e.Data, &d)
			errText = d.Message
		}
	}
	return out, errText
}

func TestForwardGrantEvents(t *testing.T) {
	deps := newTestDeps(t)
	deps.providerStore.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", APIKey: "k"})
	deps.providerStore.Register(provider.ProviderConfig{Name: "cli", Kind: provider.ProviderKindCLIClaude})
	deps.providerStore.Register(provider.ProviderConfig{Name: "codex", Kind: provider.ProviderKindCLICodex})

	if _, e := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "k", ProviderName: "nope"}); !strings.Contains(e, "unknown provider") {
		t.Fatalf("unknown provider: %q", e)
	}
	if _, e := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "k", ProviderName: "codex"}); !strings.Contains(e, "can't be used from another server") {
		t.Fatalf("codex kind: %q", e)
	}
	// A person who may not use the provider can't lend it: a limited TCP person and a CLI agent.
	tcp := deps.peers.Add(protocol.NewIPCStream(strings.NewReader(""), &capture{}), nopCloser{}, "tcp", peerIdentity{Name: "t"})
	defer deps.peers.Remove(tcp.ID)
	if _, e := grants(t, deps, "tcp", tcp.ID, protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "k", ProviderName: "cli"}); !strings.Contains(e, "isn't allowed") {
		t.Fatalf("not allowed: %q", e)
	}

	long := strings.Repeat("n", 200)
	g, e := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s2", ServerKey: "k", ServerName: "Zed", ProviderName: "p"})
	if e != "" || len(g.Grants) != 1 {
		t.Fatalf("grant: %q %+v", e, g)
	}
	grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s2", ServerKey: "k", ServerName: "Zed", ProviderName: "p"}) // idempotent
	g, _ = grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "k", ServerName: long, ProviderName: "p"})
	if n := len([]rune(g.Grants[1].ServerName)); len(g.Grants) != 2 || n == 0 || n > maxServerNameRunes || g.Grants[0].ServerName != "Zed" {
		t.Fatalf("grants (sorted by server name, label capped): %+v", g)
	}
	// Someone else's grants are never listed to this sender.
	if other, _ := grants(t, deps, "tcp", tcp.ID, protocol.EventListForwardGrants, struct{}{}); len(other.Grants) != 0 {
		t.Fatalf("the limited person sees the owner's grants: %+v", other)
	}
	// They survive a reload of the settings file.
	again, err := NewServerSettings(filepath.Dir(deps.settings.filePath))
	if err != nil {
		t.Fatal(err)
	}
	if !again.ForwardGranted(ownerID, "s2", "k", "p") {
		t.Fatal("grant lost on reload")
	}
	g, _ = grants(t, deps, "unix", "", protocol.EventForwardRevoke, protocol.ForwardRevokeData{ServerID: "s2", ProviderName: "p"})
	grants(t, deps, "unix", "", protocol.EventForwardRevoke, protocol.ForwardRevokeData{ServerID: "s2", ProviderName: "p"}) // idempotent
	if len(g.Grants) != 1 {
		t.Fatalf("after revoke: %+v", g)
	}
	// Removing the provider drops what was lent.
	deps.settings.dropForwardProvider("p")
	if g, _ = grants(t, deps, "unix", "", protocol.EventListForwardGrants, struct{}{}); len(g.Grants) != 0 {
		t.Fatalf("grants of a removed provider kept: %+v", g)
	}
	// A Claude Code kind can be lent now; the other CLI kinds can't (checked above for codex).
	if _, e2 := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "k", ProviderName: "cli"}); e2 != "" {
		t.Fatalf("cli kind: %q", e2)
	}
	if !deps.settings.ForwardGranted(ownerID, "s1", "k", "cli") {
		t.Fatal("the cli grant didn't stick")
	}
}

func TestForwardGrantsFollowPeople(t *testing.T) {
	deps := newTestDeps(t)
	s := deps.settings
	a, err := s.CreateUser("Ann", "", TierTrusted, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser("Bea", "", TierTrusted, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	s.GrantForward(a.ID, "s1", "k", "One", "p")
	s.GrantForward(b.ID, "s1", "k", "One", "q")
	if err := s.MergeUsers(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if !s.ForwardGranted(b.ID, "s1", "k", "p") || !s.ForwardGranted(b.ID, "s1", "k", "q") || s.ForwardGranted(a.ID, "s1", "k", "p") {
		t.Fatal("a merge didn't move the grants to the person kept")
	}
	if _, err := s.RemoveUser(b.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.ListForwardGrants(b.ID)) != 0 {
		t.Fatal("a removed person's grants were kept")
	}
}

func TestForwardStateOnlyForThoseWhoSeeTheChat(t *testing.T) {
	w := newFwdWorld(t)
	got := make(chan protocol.EventType, 4)
	p, pw := addTestPeer(t, w.b, "tcp")
	_ = p
	pw.set(func(e protocol.Envelope) {
		if e.Type == protocol.EventForwardState {
			got <- e.Type
		}
	})
	w.b.peers.Sees = func(network, credential, sessionID string) bool { return false }
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := w.provider(t).StreamTurn(ctx, nil, nil, "")
	<-w.states // the unix watcher saw it
	select {
	case <-got:
		t.Fatal("forward_state reached a connection that may not see the chat")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	for range chunks {
	}
	<-errs
}

// A lent Claude Code login end to end: B's provider asks through a device, A runs a (fake) claude
// as a plain model. The second request continues the first, so it must reach the same process, and
// the session key is the relaying person's id, the borrowing server and B's chat.
func TestForwardClaudeCodeLogin(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
while read -r line; do
  case "$line" in *'"type":"user"'*)
    printf '%s\n' '{"type":"system","subtype":"init","tools":["mcp__y__echo"],"mcp_servers":[{"name":"y","status":"connected"}]}'
    printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"pid '$$'"}}}'
    printf '%s\n' '{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":3,"output_tokens":2}}}'
    printf '%s\n' '{"type":"stream_event","event":{"type":"message_stop"}}'
    printf '%s\n' '{"type":"result","subtype":"success","result":"x","total_cost_usd":0.5}'
  ;; esac
done
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("YOAKELAND_STRICT_PATH", "1")
	var keys []string
	old := claudeModelFor
	claudeModelFor = func(cfg provider.ProviderConfig, model, key string) provider.Provider {
		keys = append(keys, key)
		return old(cfg, model, key)
	}
	t.Cleanup(func() { claudeModelFor = old })

	w := newFwdWorld(t)
	if err := w.a.providerStore.Register(provider.ProviderConfig{Name: "cc", Kind: provider.ProviderKindCLIClaude}); err != nil {
		t.Fatal(err)
	}
	if err := w.a.settings.GrantForward(ownerID, w.bID, bKey, "B", "cc"); err != nil {
		t.Fatal(err)
	}
	w.newDevice(t).offer(w.aID)
	p, err := newForwardedProvider(w.b, "chat1", w.aID, "cc", "haiku")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.CloseAllNativeSessions)
	ctx := agent.WithRequestHints(context.Background(), agent.RequestHints{CacheKey: "chat1"})
	tools := []tool.ToolDefinition{{Name: "echo", Description: "e", Parameters: map[string]any{"type": "object"}}}
	ask := func(h []agent.Message) (string, protocol.UsageData) {
		chunks, errs := p.StreamTurn(ctx, h, tools, "sys")
		var text string
		var u protocol.UsageData
		timeout := time.After(15 * time.Second)
		for chunks != nil {
			select {
			case c, ok := <-chunks:
				if !ok {
					chunks = nil
					break
				}
				if c.Kind == agent.StreamChunkDelta {
					text += c.Delta
				}
				if c.Kind == agent.StreamChunkUsage {
					u = *c.Usage
				}
			case <-timeout:
				t.Fatal("no answer")
			}
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		return text, u
	}
	h1 := []agent.Message{{Role: "user", Content: "one"}}
	t1, u := ask(h1)
	if !strings.HasPrefix(t1, "pid ") || u.CostUSD != 0.5 || !u.CostKnown {
		t.Fatalf("first answer %q usage %+v", t1, u)
	}
	t2, _ := ask(append(h1, agent.Message{Role: "assistant", Content: t1}, agent.Message{Role: "user", Content: "two"}))
	if t2 != t1 {
		t.Fatalf("second request ran in another process: %q vs %q", t1, t2)
	}
	want := ownerID + "\x00" + w.bID + "\x00chat1"
	if len(keys) != 2 || keys[0] != want || keys[1] != want {
		t.Fatalf("session keys %q, want %q", keys, want)
	}
	w.settled(t)
}

// A holds nothing for someone who lent nothing to the asking server, and a person's requests are
// counted across all their connections, not per connection.
func TestForwardBufferLimitsArePerPerson(t *testing.T) {
	w := newFwdWorld(t)
	piece := func(p *Peer, network, id string) {
		raw, _ := json.Marshal(protocol.ForwardRequestData{RequestID: id, ServerID: w.aID, From: w.bID, FromKey: bKey, Data: strings.Repeat("a", 1<<20)})
		handleForwardEnvelope(context.Background(), p.ipc, w.a, network, p.ID, &protocol.Envelope{Type: protocol.EventForwardRequest, Data: raw})
	}
	conn := func(network string) (*Peer, *capture) {
		c := &capture{}
		p := w.a.peers.Add(protocol.NewIPCStream(strings.NewReader(""), c), nopCloser{}, network, peerIdentity{Name: "x"})
		t.Cleanup(func() { w.a.forward.connClosed(w.a, p.ipc, p.ID); w.a.peers.Remove(p.ID) })
		return p, c
	}
	stranger, sc := conn("tcp")
	piece(stranger, "tcp", "s")
	if e := sc.end(t, "s"); e != notLent {
		t.Fatalf("stranger's first piece: %q", e)
	}
	w.a.forward.mu.Lock()
	held := len(w.a.forward.conns)
	w.a.forward.mu.Unlock()
	if held != 0 {
		t.Fatal("a stranger's piece was held")
	}
	one, _ := conn("unix")
	for i := range maxForwardInFlight {
		piece(one, "unix", fmt.Sprint("r", i))
	}
	two, c2 := conn("unix")
	piece(two, "unix", "more")
	if e := c2.end(t, "more"); !strings.Contains(e, "for this person") {
		t.Fatalf("the owner's ninth request on a second connection: %q", e)
	}
}

// Taking a grant back stops what is already running under it.
func TestForwardRevokeStopsRunningRequests(t *testing.T) {
	w := newFwdWorld(t)
	w.model.handler = w.model.hold(nil)
	w.newDevice(t).offer(w.aID)
	chunks, errs := w.provider(t).StreamTurn(context.Background(), []agent.Message{{Role: "user", Content: "hi"}}, nil, "")
	if c := <-chunks; c.Delta != "he" {
		t.Fatalf("first chunk %+v", c)
	}
	if _, e := grants(t, w.a, "unix", "", protocol.EventForwardRevoke, protocol.ForwardRevokeData{ServerID: w.bID, ProviderName: "p"}); e != "" {
		t.Fatal(e)
	}
	if _, _, err := collect(t, chunks, errs); err == nil {
		t.Fatal("a revoked request finished as an answer")
	}
	select {
	case <-w.model.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("A's provider kept running after the grant was revoked")
	}
	w.settled(t)
}

// B hears the provider's own words, not A's provider address.
func TestForwardErrorsLeaveOutTheProviderAddress(t *testing.T) {
	cfg := provider.ProviderConfig{BaseURL: "https://llm.internal:8443/v1"}
	got := forwardErrText(cfg, fmt.Errorf(`Post "https://llm.internal:8443/v1/chat/completions": dial tcp: lookup llm.internal: no such host`))
	if strings.Contains(got, "llm.internal") || !strings.Contains(got, "no such host") {
		t.Fatalf("scrubbed to %q", got)
	}
}

// A refused grant is answered with the list too, so a device shows what the lender has, and a grant
// is kept with the key the device reached the server by.
func TestForwardGrantRefusalSendsTheList(t *testing.T) {
	deps := newTestDeps(t)
	deps.providerStore.Register(provider.ProviderConfig{Name: "p", Kind: provider.ProviderKindAPIKey, HTTPProvider: "openai", APIKey: "k"})
	if _, e := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "ca:1", ProviderName: "p"}); e != "" {
		t.Fatal(e)
	}
	g, e := grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ServerKey: "ca:1", ProviderName: "nope"})
	if e == "" || len(g.Grants) != 1 || !reflect.DeepEqual(g.Grants[0].ServerKeys, []string{"ca:1"}) {
		t.Fatalf("refused grant answered with %+v, error %q", g, e)
	}
	if g, e = grants(t, deps, "unix", "", protocol.EventForwardGrant, protocol.ForwardGrantData{ServerID: "s1", ProviderName: "p"}); e == "" || len(g.Grants) != 1 {
		t.Fatalf("a grant with no key: %+v, error %q", g, e)
	}
}
