package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"yoakeland/internal/agent"
	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
	"yoakeland/internal/tool"
)

// Model forwarding (PLAN_FORWARDING, SPEC.md "Model forwarding"). A chat on this server (B) can run
// on a provider that lives on another one (A): B's loop, tools and history stay here and only the
// model request goes to A, through a device connected to both. The device is a pipe; nothing about
// the request is stored here or on the device.
//
// As A: answerForward. As B: ForwardedProvider, which asks a device to carry the request.

const (
	maxForwardInFlight = 8   // requests a connection may have open at once on A
	maxForwardIDLen    = 128 // longer request ids are refused: they are map keys
	maxRelayServers    = 64  // servers one relay_offer may name
	forwardMsgBuffer   = 256 // chunks B holds for a turn that reads slower than the device sends
	viaPrefix          = "via:"
)

// forwardWaitLimit is how long B waits for a device connected to both servers.
var forwardWaitLimit = 30 * time.Minute

// forwardableKind is the one place that says which kinds of provider may be used from another
// server. A CLI agent runs its own tools on its own machine, so it stays out until phase 3.
func forwardableKind(k provider.ProviderKind) bool {
	return k == provider.ProviderKindAPIKey
}

// parseVia splits the name B gives a borrowed provider, via:<serverId>:<name>.
func parseVia(s string) (serverID, name string, ok bool) {
	rest, ok := strings.CutPrefix(s, viaPrefix)
	if !ok {
		return "", "", false
	}
	serverID, name, ok = strings.Cut(rest, ":")
	return serverID, name, ok && serverID != "" && name != ""
}

// forwardBody is the request a ForwardedProvider sends and A answers, as JSON text.
type forwardBody struct {
	ProviderName string                `json:"providerName"`
	Model        string                `json:"model"`
	Messages     []agent.Message       `json:"messages"`
	Tools        []tool.ToolDefinition `json:"tools"`
	SystemPrompt string                `json:"systemPrompt"`
	Hints        agent.RequestHints    `json:"hints"`
	Options      agent.ModelOptions    `json:"options"`
}

func toWireChunk(c agent.ModelStreamChunk) (protocol.ForwardChunk, bool) {
	w := protocol.ForwardChunk{Kind: string(c.Kind), Delta: c.Delta, Usage: c.Usage, FinishReason: c.FinishReason, Raw: c.Raw}
	if t := c.ToolCall; t != nil {
		w.ToolCall = &protocol.ForwardToolCall{ID: t.ID, Name: t.Name, Args: t.Args, NativeToolInfo: t.NativeToolInfo}
	}
	if x := c.Context; x != nil {
		w.Context = &protocol.ForwardContext{ContextTokens: x.ContextTokens, WindowTokens: x.WindowTokens, Source: x.Source}
	}
	// A native agent's own tool results and compactions only come from CLI agents, which aren't forwarded.
	return w, c.Kind != agent.StreamChunkToolResult && c.Kind != agent.StreamChunkCompaction
}

func fromWireChunk(w protocol.ForwardChunk) agent.ModelStreamChunk {
	c := agent.ModelStreamChunk{Kind: agent.StreamEventKind(w.Kind), Delta: w.Delta, Usage: w.Usage, FinishReason: w.FinishReason, Raw: w.Raw}
	if t := w.ToolCall; t != nil {
		c.ToolCall = &agent.ToolCall{ID: t.ID, Name: t.Name, Args: t.Args, NativeToolInfo: t.NativeToolInfo}
	}
	if x := w.Context; x != nil {
		c.Context = &agent.ContextSnapshot{ContextTokens: x.ContextTokens, WindowTokens: x.WindowTokens, Source: x.Source}
	}
	return c
}

// splitPieces cuts body into pieces of at most size bytes without splitting a character, since a
// piece travels as a JSON string and a broken character would not survive it.
func splitPieces(body []byte, size int) []string {
	var out []string
	for len(body) > size {
		n := size
		for n > 0 && !utf8.RuneStart(body[n]) {
			n--
		}
		out, body = append(out, string(body[:n])), body[n:]
	}
	return append(out, string(body))
}

// forwarder holds what forwarding keeps in memory, for both roles. Every entry goes when its
// request or connection does.
type forwarder struct {
	mu    sync.Mutex
	conns map[*protocol.IPCStream]*fwdConn // A: requests being answered, by the connection they came on
	calls map[string]*fwdCall              // B: requests handed to a device, by request id
	// changed is closed (and replaced) whenever a device's offer or presence changes, to wake a
	// turn that is waiting for one.
	changed chan struct{}
}

func newForwarder() *forwarder {
	return &forwarder{conns: map[*protocol.IPCStream]*fwdConn{}, calls: map[string]*fwdCall{}, changed: make(chan struct{})}
}

type fwdConn struct{ reqs map[string]*fwdReq }

// fwdReq is a request on A: pieces being collected, then (cancel set) a provider call running.
type fwdReq struct {
	from   string // the server the request came from, as the device stamped it on piece 0
	buf    []byte
	next   int
	cancel context.CancelFunc
}

// fwdCall is a request B gave a device. msgs is what the device sent back for it.
type fwdCall struct {
	peerID   string
	msgs     chan fwdMsg
	done     chan struct{} // closed when the turn stops listening
	lost     chan struct{} // closed when the device's connection ended
	lostOnce sync.Once
}

type fwdMsg struct {
	chunk *protocol.ForwardChunk
	end   *protocol.ForwardEndData
}

func (f *forwarder) changes() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *forwarder) notify() {
	f.mu.Lock()
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// SetRelayOffer records the servers the peer id says it is connected to, replacing its last offer.
func (r *PeerRegistry) SetRelayOffer(id string, servers []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if ok {
		p.relayTo = servers
	}
	return ok
}

// relayPeers are the connections that offered to reach serverID and may read the chat sessionID.
func (r *PeerRegistry) relayPeers(sessionID, serverID string) []*Peer {
	r.mu.Lock()
	var out []*Peer
	for _, p := range r.peers {
		if slices.Contains(p.relayTo, serverID) {
			out = append(out, p)
		}
	}
	r.mu.Unlock()
	return slices.DeleteFunc(out, func(p *Peer) bool { return !r.canSee(p, sessionID) })
}

// connClosed lets go of everything the connection ipc held: the requests it was having answered
// stop, and a request B had given it as a device ends as "lost".
func (f *forwarder) connClosed(deps connDeps, ipc *protocol.IPCStream, peerID string) {
	if f == nil {
		return
	}
	// The offer goes first, so a turn woken by the loss doesn't pick this connection again.
	if peerID != "" {
		deps.peers.SetRelayOffer(peerID, nil)
	}
	f.mu.Lock()
	if c := f.conns[ipc]; c != nil {
		for _, r := range c.reqs {
			if r.cancel != nil {
				r.cancel()
			}
		}
		delete(f.conns, ipc)
	}
	for _, c := range f.calls {
		if peerID != "" && c.peerID == peerID {
			c.lostOnce.Do(func() { close(c.lost) })
		}
	}
	f.mu.Unlock()
	f.notify()
}

// handleForwardEnvelope answers the forwarding events. A device may send any of them, so each
// handler narrows to what the connection is entitled to.
func handleForwardEnvelope(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, env *protocol.Envelope) bool {
	if handleGrantEnvelope(ipc, deps, network, peerID, env) {
		return true
	}
	switch env.Type {
	case protocol.EventRelayOffer, protocol.EventForwardRequest, protocol.EventForwardCancel, protocol.EventForwardChunk, protocol.EventForwardEnd:
	default:
		return false
	}
	f := deps.forward
	if f == nil {
		if env.Type == protocol.EventForwardRequest {
			var d protocol.ForwardRequestData
			_ = json.Unmarshal(env.Data, &d)
			sendForwardEnd(ipc, d.RequestID, "this server doesn't forward models")
		}
		return true
	}
	var err error
	switch env.Type {
	case protocol.EventRelayOffer:
		var d protocol.RelayOfferData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			servers := make([]string, 0, len(d.Servers))
			for _, s := range d.Servers {
				if s != "" && len(s) <= maxForwardIDLen && len(servers) < maxRelayServers {
					servers = append(servers, s)
				}
			}
			if deps.peers.SetRelayOffer(peerID, servers) {
				f.notify()
			}
		}
	case protocol.EventForwardRequest:
		var d protocol.ForwardRequestData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			f.accept(ctx, ipc, deps, network, peerID, d)
		}
	case protocol.EventForwardCancel:
		var d protocol.ForwardCancelData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			f.drop(ipc, d.RequestID)
		}
	case protocol.EventForwardChunk:
		var d protocol.ForwardChunkData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			f.deliver(peerID, d.RequestID, fwdMsg{chunk: &d.Chunk})
		}
	case protocol.EventForwardEnd:
		var d protocol.ForwardEndData
		if err = json.Unmarshal(env.Data, &d); err == nil {
			f.deliver(peerID, d.RequestID, fwdMsg{end: &d})
		}
	}
	if err != nil {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: fmt.Sprintf("malformed %s payload", env.Type)})
	}
	return true
}

func sendForwardEnd(ipc *protocol.IPCStream, requestID, errText string) {
	_ = ipc.SendEvent(protocol.EventForwardEnd, "", "", "", protocol.ForwardEndData{RequestID: requestID, Error: errText})
}

// ---- As A ----

// accept collects one piece of a request and, on the last, starts answering it. Every refusal is a
// forward_end with the reason, and drops what the request had collected.
func (f *forwarder) accept(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID string, d protocol.ForwardRequestData) {
	refuse := func(why string) {
		f.drop(ipc, d.RequestID)
		sendForwardEnd(ipc, d.RequestID, why)
	}
	if d.RequestID == "" || len(d.RequestID) > maxForwardIDLen {
		sendForwardEnd(ipc, d.RequestID, "forward_request needs a requestId of at most 128 characters")
		return
	}
	if d.ServerID != deps.settings.GetServerID() {
		refuse("this server is not " + d.ServerID)
		return
	}
	var body []byte
	var from string
	var rctx context.Context
	why := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.conns[ipc]
		if c == nil {
			c = &fwdConn{reqs: map[string]*fwdReq{}}
			f.conns[ipc] = c
		}
		r := c.reqs[d.RequestID]
		switch {
		case r == nil && d.Seq != 0:
			return "forward_request pieces out of order"
		case r == nil && len(c.reqs) >= maxForwardInFlight:
			return fmt.Sprintf("more than %d forwarded requests in flight on this connection", maxForwardInFlight)
		case r != nil && (r.cancel != nil || d.Seq != r.next):
			return "forward_request pieces out of order"
		case r == nil:
			r = &fwdReq{from: d.From}
			c.reqs[d.RequestID] = r
		}
		if len(r.buf)+len(d.Data) > protocol.MaxForwardBody {
			return "forwarded request body is over 64 MiB"
		}
		r.buf = append(r.buf, d.Data...)
		r.next++
		if d.Last {
			var cancel context.CancelFunc
			rctx, cancel = context.WithCancel(ctx)
			body, from, r.buf, r.cancel = r.buf, r.from, nil, cancel
		}
		return ""
	}()
	if why != "" {
		refuse(why)
		return
	}
	if !d.Last {
		return
	}
	if deps.turnWG != nil {
		deps.turnWG.Add(1)
	}
	go f.answer(rctx, ipc, deps, network, peerID, d.RequestID, from, body)
}

// drop forgets request id on ipc and stops it if it is running.
func (f *forwarder) drop(ipc *protocol.IPCStream, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.conns[ipc]
	if c == nil {
		return
	}
	if r := c.reqs[id]; r != nil {
		if r.cancel != nil {
			r.cancel()
		}
		delete(c.reqs, id)
	}
	if len(c.reqs) == 0 {
		delete(f.conns, ipc)
	}
}

// answer runs the request on A's own provider as the person behind the relaying connection and
// streams the result back, then closes the request. ctx ends it: forward_cancel, or the connection.
func (f *forwarder) answer(ctx context.Context, ipc *protocol.IPCStream, deps connDeps, network, peerID, id, from string, raw []byte) {
	if deps.turnWG != nil {
		defer deps.turnWG.Done()
	}
	var b forwardBody
	outcome, errText := "ok", ""
	var in, out int
	defer func() {
		if r := recover(); r != nil {
			logError("forwarded request %s panicked: %v", id, r)
			errText = fmt.Sprintf("internal error: %v", r)
		}
		if errText != "" {
			outcome = "error"
		}
		if ctx.Err() != nil {
			outcome = "cancelled"
			if errText == "" {
				errText = "cancelled"
			}
		}
		f.drop(ipc, id)
		sendForwardEnd(ipc, id, errText)
		// Never the conversation or the key: which provider and model, and what it cost in tokens.
		deps.record(ipc, peerID, audit.Event{Kind: audit.ProviderForward, Target: b.ProviderName, Outcome: outcome, Detail: map[string]any{
			"model": b.Model, "inputTokens": in, "outputTokens": out,
		}})
	}()

	if err := json.Unmarshal(raw, &b); err != nil {
		errText = "malformed forwarded request"
		return
	}
	cfg, ok := deps.providerStore.Get(b.ProviderName)
	acc := deps.accessFor(network, peerID)
	switch {
	case !ok:
		errText = fmt.Sprintf("unknown provider %q", b.ProviderName)
	case !providerAllowed(deps, acc, b.ProviderName):
		errText = fmt.Sprintf("provider %q isn't allowed for this person", b.ProviderName)
	case !forwardableKind(cfg.Kind):
		errText = fmt.Sprintf("provider %q can't be used from another server", b.ProviderName)
	// Lending is the person's own choice, made on this server: a server they are only a guest on
	// can't spend their keys by asking.
	case !deps.settings.ForwardGranted(acc.UserID, from, b.ProviderName):
		errText = fmt.Sprintf("%s isn't lent to that server. Pick it in the model picker there to allow it.", b.ProviderName)
	}
	if errText != "" {
		return
	}
	built, err := provider.BuildProvider(cfg, b.Model, "")
	if err != nil {
		errText = fmt.Sprintf("failed to activate provider %q: %v", b.ProviderName, err)
		return
	}
	if s, ok := built.(agent.ModelOptionsSetter); ok {
		s.SetModelOptions(b.Options)
	}
	// ponytail: A's per-session spend caps and usage store don't see forwarded use (the store is
	// per project and keyed by chat; there is none here). A pseudo-session per person if that matters.
	chunks, errs := built.StreamTurn(agent.WithRequestHints(ctx, b.Hints), b.Messages, b.Tools, b.SystemPrompt)
	for c := range chunks {
		if c.Kind == agent.StreamChunkUsage && c.Usage != nil {
			in, out = in+c.Usage.InputTokens, out+c.Usage.OutputTokens
		}
		w, ok := toWireChunk(c)
		if !ok {
			continue
		}
		if ipc.SendEvent(protocol.EventForwardChunk, "", "", "", protocol.ForwardChunkData{RequestID: id, Chunk: w}) != nil {
			f.drop(ipc, id) // the device is gone: stop the provider, and drain until it closes
		}
	}
	if err := <-errs; err != nil {
		errText = err.Error()
	}
}

// ---- As B ----

// ForwardedProvider is a provider that lives on another server, reached through a device. It is
// bound under the name via:<serverId>:<name>.
type ForwardedProvider struct {
	deps      connDeps
	sessionID string
	serverID  string
	name      string // A's name for the provider
	model     string

	mu   sync.Mutex
	opts agent.ModelOptions
}

func newForwardedProvider(deps connDeps, sessionID, serverID, name, model string) (*ForwardedProvider, error) {
	switch {
	case deps.forward == nil:
		return nil, errors.New("this server doesn't forward models")
	case serverID == deps.settings.GetServerID():
		return nil, fmt.Errorf("provider %q is on this server; use it directly", name)
	}
	return &ForwardedProvider{deps: deps, sessionID: sessionID, serverID: serverID, name: name, model: model}, nil
}

func (p *ForwardedProvider) ModelName() string { return p.model }

// SupportsImages is decided from the model name, as the HTTP providers do.
func (p *ForwardedProvider) SupportsImages() bool { return provider.SupportsImages(p.model) }

// SetModelOptions merges as the HTTP providers do: an empty field leaves the last value, "default"
// goes back to the model's own.
func (p *ForwardedProvider) SetModelOptions(o agent.ModelOptions) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch o.ReasoningEffort {
	case "":
	case "default":
		p.opts.ReasoningEffort, p.opts.ThinkingBudget = "", 0
	default:
		p.opts.ReasoningEffort = o.ReasoningEffort
	}
	if o.ThinkingBudget != 0 {
		p.opts.ThinkingBudget = o.ThinkingBudget
	}
	if o.FastMode != nil {
		p.opts.FastMode = o.FastMode
	}
	if o.AutoCompactTokens != nil {
		p.opts.AutoCompactTokens = o.AutoCompactTokens
	}
	if o.AutoCompact != nil {
		p.opts.AutoCompact = o.AutoCompact
	}
}

// GenerateTitle is an ordinary request through the device, as the HTTP providers' own is a plain
// request to their API.
func (p *ForwardedProvider) GenerateTitle(ctx context.Context, prompt string) (string, error) {
	chunks, errs := p.StreamTurn(ctx, []agent.Message{{Role: "user", Content: prompt}}, nil, provider.TitleSystemPrompt)
	var sb strings.Builder
	for c := range chunks {
		if c.Kind == agent.StreamChunkDelta {
			sb.WriteString(c.Delta)
		}
	}
	if err := <-errs; err != nil {
		return "", err
	}
	if t := strings.TrimSpace(sb.String()); t != "" {
		return t, nil
	}
	return "", errors.New("no text in the title response")
}

// StreamTurn closes both channels when it ends, with the failure (if any) already in errs, like
// the HTTP providers: the turn loop reads a closed chunk channel with a buffered error as a failure.
func (p *ForwardedProvider) StreamTurn(ctx context.Context, history []agent.Message, tools []tool.ToolDefinition, systemPrompt string) (<-chan agent.ModelStreamChunk, <-chan error) {
	chunks := make(chan agent.ModelStreamChunk, 64)
	errs := make(chan error, 1)
	p.mu.Lock()
	opts := p.opts
	p.mu.Unlock()
	body, err := json.Marshal(forwardBody{
		ProviderName: p.name, Model: p.model, Messages: history, Tools: tools, SystemPrompt: systemPrompt,
		Hints: agent.RequestHintsFrom(ctx), Options: opts,
	})
	go func() {
		defer close(chunks)
		defer close(errs)
		if err == nil {
			err = p.run(ctx, body, chunks)
		}
		if err != nil {
			errs <- err
		}
	}()
	return chunks, errs
}

type relayPreferKey struct{}

// withPreferredRelay names the device a turn should try first: the one that started it.
func withPreferredRelay(ctx context.Context, peerID string) context.Context {
	return context.WithValue(ctx, relayPreferKey{}, peerID)
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return randomID() + randomID()
	}
	return hex.EncodeToString(b)
}

// pick chooses the device for a request: the preferred one if it qualifies, else the most
// recently connected.
func (f *forwarder) pick(deps connDeps, sessionID, serverID, prefer string) *Peer {
	ps := deps.peers.relayPeers(sessionID, serverID)
	if i := slices.IndexFunc(ps, func(p *Peer) bool { return p.ID == prefer }); i >= 0 {
		return ps[i]
	}
	if len(ps) == 0 {
		return nil
	}
	return slices.MaxFunc(ps, func(a, b *Peer) int { return a.ConnectedAt.Compare(b.ConnectedAt) })
}

func (f *forwarder) addCall(id, peerID string) *fwdCall {
	c := &fwdCall{peerID: peerID, msgs: make(chan fwdMsg, forwardMsgBuffer), done: make(chan struct{}), lost: make(chan struct{})}
	f.mu.Lock()
	f.calls[id] = c
	f.mu.Unlock()
	return c
}

func (f *forwarder) removeCall(id string, c *fwdCall) {
	f.mu.Lock()
	delete(f.calls, id)
	f.mu.Unlock()
	close(c.done)
}

// deliver hands what a device sent back to the request it belongs to, if that request was given to
// this very device. Anything else is dropped.
func (f *forwarder) deliver(peerID, id string, m fwdMsg) {
	f.mu.Lock()
	c := f.calls[id]
	f.mu.Unlock()
	if c == nil || c.peerID != peerID {
		logWarn("dropped a forwarded reply for request %q from a device that wasn't given it", id)
		return
	}
	// ponytail: a turn that stalls with the buffer full holds up this device's other events;
	// a drop-and-fail policy if that ever shows.
	select {
	case c.msgs <- m:
	case <-c.done:
	}
}

func (p *ForwardedProvider) announce(state string) {
	p.deps.peers.BroadcastEvent("", protocol.EventForwardState, p.sessionID, protocol.ForwardStateData{ServerID: p.serverID, State: state})
}

// run gets the request answered: it finds a device, waits for one if there is none, and streams
// what comes back into out. A device lost before anything reached the turn is replaced; after
// that the request fails, since a half-streamed answer is never replayed.
func (p *ForwardedProvider) run(ctx context.Context, body []byte, out chan<- agent.ModelStreamChunk) error {
	f := p.deps.forward
	prefer, _ := ctx.Value(relayPreferKey{}).(string)
	// Each wait gets the whole limit: the timer starts when a wait does.
	var timer *time.Timer
	var limit <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	waiting, sent := false, false
	for {
		changed := f.changes()
		dev := f.pick(p.deps, p.sessionID, p.serverID, prefer)
		if dev == nil {
			if !waiting {
				waiting = true
				timer = time.NewTimer(forwardWaitLimit)
				limit = timer.C
				p.announce(protocol.ForwardWaiting)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-limit:
				return errors.New("no device connected to both this server and the one that has this provider came online")
			case <-changed:
			}
			continue
		}
		if waiting {
			waiting = false
			timer.Stop()
			p.announce(protocol.ForwardRelaying)
		}
		retry, err := p.relay(ctx, dev, body, out, &sent)
		if !retry {
			return err
		}
	}
}

// relay gives the request to dev and carries the reply into out. retry is true when dev went away
// before anything reached the turn.
func (p *ForwardedProvider) relay(ctx context.Context, dev *Peer, body []byte, out chan<- agent.ModelStreamChunk, sent *bool) (retry bool, err error) {
	f := p.deps.forward
	id := newRequestID()
	call := f.addCall(id, dev.ID)
	defer f.removeCall(id, call)
	pieces := splitPieces(body, protocol.ForwardPieceSize)
	for i, piece := range pieces {
		d := protocol.ForwardRequestData{RequestID: id, ServerID: p.serverID, Seq: i, Last: i == len(pieces)-1, Data: piece}
		if dev.ipc.SendEvent(protocol.EventForwardRequest, p.sessionID, "", "", d) != nil {
			// A connection that can't be written to offers nothing, so it isn't picked again.
			p.deps.peers.SetRelayOffer(dev.ID, nil)
			return !*sent, p.lostErr()
		}
	}
	// handle passes one reply on; done is true once the request has ended.
	handle := func(m fwdMsg) (done bool, err error) {
		if m.end != nil {
			if m.end.Error != "" {
				return true, errors.New(m.end.Error)
			}
			return true, nil
		}
		select {
		case out <- fromWireChunk(*m.chunk):
			*sent = true
			return false, nil
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
	for {
		select {
		case <-ctx.Done():
			_ = dev.ipc.SendEvent(protocol.EventForwardCancel, "", "", "", protocol.ForwardCancelData{RequestID: id})
			return false, ctx.Err()
		case m := <-call.msgs:
			if done, err := handle(m); done {
				if ctx.Err() != nil {
					_ = dev.ipc.SendEvent(protocol.EventForwardCancel, "", "", "", protocol.ForwardCancelData{RequestID: id})
				}
				return false, err
			}
		case <-call.lost:
			// What the device sent before it went still counts.
			for {
				select {
				case m := <-call.msgs:
					if done, err := handle(m); done {
						return false, err
					}
					continue
				default:
				}
				break
			}
			return !*sent, p.lostErr()
		}
	}
}

func (p *ForwardedProvider) lostErr() error {
	return errors.New("the device relaying to the server that has this provider disconnected; send a message to continue")
}
