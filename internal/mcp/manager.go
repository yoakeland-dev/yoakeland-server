package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/tool"
)

// Scope is where a server was configured.
type Scope string

const (
	ScopeUser    Scope = "user"    // the user's own list, every project
	ScopeProject Scope = "project" // the project's .mcp.json, which has to be approved
)

// State is where a server is in its life.
type State string

const (
	StateIdle       State = "idle"       // configured, not started (it starts when a turn needs it)
	StateDisabled   State = "disabled"   // switched off, or a project server the user refused
	StatePending    State = "pending"    // a project server nobody approved yet
	StateConnecting State = "connecting" //
	StateReady      State = "ready"
	StateNeedsAuth  State = "needs_auth"
	StateFailed     State = "failed"
)

const (
	// maxToolsPerServer bounds what one server can add to every request.
	maxToolsPerServer = 200
	connectTimeout    = 60 * time.Second
)

// entry is one server of one project.
type entry struct {
	mgr   *Manager
	name  string
	scope Scope
	cfg   ServerConfig
	hash  string

	// Guarded by mgr.mu.
	expandErr string
	approval  string // project servers: "approved", "pending", "changed" or "denied"
	state     State
	err       string
	diag      string
	client    *Client
	info      ServerInfo
	tools     []*mcpTool
	toolInfos []ToolInfo
	resources []ResourceInfo
	resTool   *resourceTool
	truncated int
	failures  int
	retryAt   time.Time
	challenge *errNeedsAuth
	settled   chan struct{} // closed when the connect attempt in flight ends
	stale     bool          // the server said its tool list changed
	listedAt  time.Time
}

// Manager runs one project's MCP servers: the user's own, and the project's
// .mcp.json ones the user approved. It is the tool.Source an agent reads.
type Manager struct {
	root   string
	store  *Store
	auth   *AuthStore
	flow   *Flow
	notify func()
	getenv func(string) (string, bool)

	mu       sync.Mutex
	entries  map[string]*entry
	problems []string
	fileSig  string
	fileSrv  map[string]ServerConfig
	fileProb []string
	closed   bool
}

// NewManager serves the project at root. notify is called (never under a lock)
// whenever a server's status or tool list changes.
func NewManager(root string, store *Store, auth *AuthStore, flow *Flow, notify func()) *Manager {
	if notify == nil {
		notify = func() {}
	}
	return &Manager{root: root, store: store, auth: auth, flow: flow, notify: notify, entries: map[string]*entry{},
		getenv: func(k string) (string, bool) { v, ok := os.LookupEnv(k); return v, ok }}
}

// Root is the project this manager serves.
func (m *Manager) Root() string { return m.root }

func (m *Manager) projectServers() (map[string]ServerConfig, []string) {
	path := filepath.Join(m.root, ProjectFile)
	sig := "none"
	if fi, err := os.Stat(path); err == nil {
		sig = fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
	}
	if m.fileSrv == nil || sig != m.fileSig {
		m.fileSrv, m.fileProb = LoadProjectFile(m.root)
		m.fileSig = sig
	}
	return m.fileSrv, m.fileProb
}

// reload brings the entries in line with the configuration: a changed server
// is replaced (and its old process stopped), a removed one is dropped. Called
// with m.mu held; it returns the clients to close once the lock is released.
func (m *Manager) reload() (stop []*Client) {
	user := m.store.Servers()
	proj, problems := m.projectServers()
	m.problems = append([]string(nil), problems...)
	want := map[string]struct {
		cfg   ServerConfig
		scope Scope
	}{}
	for n, c := range user {
		want[n] = struct {
			cfg   ServerConfig
			scope Scope
		}{c, ScopeUser}
	}
	for n, c := range proj {
		if _, dup := want[n]; dup {
			m.problems = append(m.problems, fmt.Sprintf("%s: server %q is shadowed by your own server of that name", ProjectFile, n))
			continue
		}
		want[n] = struct {
			cfg   ServerConfig
			scope Scope
		}{c, ScopeProject}
	}
	for name, e := range m.entries {
		w, ok := want[name]
		if !ok || w.scope != e.scope || w.cfg.Hash() != e.hash {
			if e.client != nil {
				stop = append(stop, e.client)
			}
			delete(m.entries, name)
		}
	}
	for name, w := range want {
		e := m.entries[name]
		if e == nil {
			e = &entry{mgr: m, name: name, scope: w.scope, cfg: w.cfg, hash: w.cfg.Hash(), state: StateIdle}
			m.entries[name] = e
		}
		if e.state == StateReady && !sameStrings(e.cfg.DisabledTools, w.cfg.DisabledTools) {
			e.cfg = w.cfg
			m.setTools(e, e.toolInfos, e.resources)
		}
		e.cfg = w.cfg
		e.approval = ""
		if e.scope == ScopeProject {
			ap, ok := m.store.Approval(m.root, name)
			switch {
			case !ok:
				e.approval = "pending"
			case ap.Hash != e.hash:
				e.approval = "changed"
			case ap.Approved:
				e.approval = "approved"
			default:
				e.approval = "denied"
			}
		}
		if _, err := w.cfg.Expanded(m.envFor(e)); err != nil {
			e.expandErr = err.Error()
		} else {
			e.expandErr = ""
		}
		switch {
		case e.cfg.Disabled || e.approval == "denied":
			e.state = StateDisabled
			if e.client != nil {
				stop = append(stop, e.client)
				e.client, e.tools, e.toolInfos, e.resources, e.resTool = nil, nil, nil, nil, nil
			}
		case e.approval == "pending" || e.approval == "changed":
			e.state = StatePending
		case e.state == StateDisabled || e.state == StatePending:
			e.state = StateIdle
		}
	}
	return stop
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func closeAll(clients []*Client) {
	for _, c := range clients {
		c.Close()
	}
}

// startable: the server may be started now.
func (e *entry) startable() bool {
	return e.state != StateDisabled && e.state != StatePending && e.expandErr == ""
}

// Prepare starts every server that isn't running and waits, bounded by ctx, for
// them to settle, so a turn's first request already has their tools. A server
// that failed is not retried until its backoff is over.
func (m *Manager) Prepare(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	stop := m.reload()
	var wait []chan struct{}
	var relist []*entry
	for _, e := range m.entries {
		if !e.startable() {
			continue
		}
		switch e.state {
		case StateReady:
			if e.stale || (e.client != nil && e.client.Info().Protocol == protoModern && time.Since(e.listedAt) > 5*time.Minute) {
				relist = append(relist, e)
			}
		case StateConnecting:
			wait = append(wait, e.settled)
		default:
			if time.Now().After(e.retryAt) {
				wait = append(wait, m.startLocked(e))
			}
		}
	}
	m.mu.Unlock()
	closeAll(stop)
	for _, e := range relist {
		m.relist(ctx, e)
	}
	for _, ch := range wait {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// startLocked begins connecting e in the background and returns the channel
// that closes when that attempt ends. m.mu is held.
func (m *Manager) startLocked(e *entry) chan struct{} {
	if e.state == StateConnecting && e.settled != nil {
		return e.settled
	}
	e.state, e.err, e.diag = StateConnecting, "", ""
	e.settled = make(chan struct{})
	settled := e.settled
	go m.connect(e, e.cfg, settled) // cfg read here, under the lock: reload rewrites it
	go m.notify()
	return settled
}

func (m *Manager) connect(e *entry, raw ServerConfig, settled chan struct{}) {
	defer close(settled)
	cfg, err := raw.Expanded(m.envFor(e))
	if err != nil {
		m.finish(e, settled, nil, err, nil, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	opt := Options{Config: cfg, Dir: m.root, OnToolsChanged: func() {
		m.mu.Lock()
		e.stale = true
		m.mu.Unlock()
		m.notify()
	}}
	if cfg.EffectiveType() != TypeStdio && m.auth != nil {
		opt.Bearer = m.auth.Bearer(cfg.URL)
	}
	c, err := Dial(ctx, opt)
	if err != nil {
		m.finish(e, settled, nil, err, nil, nil)
		return
	}
	infos, err := c.ListTools(ctx)
	if err != nil {
		diag := c.Diag()
		c.Close()
		if diag != "" && !strings.Contains(err.Error(), diag) {
			err = fmt.Errorf("%w\n%s", err, diag)
		}
		m.finish(e, settled, nil, err, nil, nil)
		return
	}
	var res []ResourceInfo
	if c.Info().Resources {
		if r, rerr := c.ListResources(ctx); rerr == nil {
			res = r
		}
	}
	m.finish(e, settled, c, nil, infos, res)
}

// finish records the outcome of a connect attempt, ignoring one that has been
// superseded (the entry was replaced, or restarted, while it ran).
func (m *Manager) finish(e *entry, settled chan struct{}, c *Client, err error, infos []ToolInfo, res []ResourceInfo) {
	m.mu.Lock()
	if m.closed || m.entries[e.name] != e || e.settled != settled || e.state != StateConnecting {
		m.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return
	}
	var auth *errNeedsAuth
	switch {
	case err == nil:
		e.client, e.info, e.state, e.err, e.diag, e.failures, e.stale = c, c.Info(), StateReady, "", "", 0, false
		e.challenge = nil
		m.setTools(e, infos, res)
		e.listedAt = time.Now()
		go m.watch(e, c)
	case errors.As(err, &auth):
		e.state, e.err, e.challenge = StateNeedsAuth, "this server needs sign-in", auth
		e.client, e.tools, e.resTool = nil, nil, nil
	default:
		e.state, e.err = StateFailed, err.Error()
		e.failures++
		back := time.Duration(min(1<<min(e.failures, 6), 600)) * 15 * time.Second / 2 // 15s, 30s, ... capped near 10 min
		e.retryAt = time.Now().Add(back)
		e.client, e.tools, e.resTool = nil, nil, nil
	}
	m.mu.Unlock()
	m.notify()
}

// setTools builds e's tools from what the server listed. m.mu is held.
func (m *Manager) setTools(e *entry, infos []ToolInfo, res []ResourceInfo) {
	disabled := map[string]bool{}
	for _, n := range e.cfg.DisabledTools {
		disabled[n] = true
	}
	taken := map[string]bool{}
	e.toolInfos, e.tools, e.truncated, e.resTool, e.resources = infos, nil, 0, nil, res
	for _, info := range infos {
		if disabled[info.Name] || info.Name == "" {
			continue
		}
		schema, err := normalizeSchema(info.InputSchema)
		if err != nil {
			continue // one bad tool must not cost the server its others
		}
		if len(e.tools) >= maxToolsPerServer {
			e.truncated++
			continue
		}
		desc := strings.TrimSpace(info.Description)
		if desc == "" {
			desc = info.Title
		}
		if len(desc) > maxDescription {
			desc = strings.ToValidUTF8(desc[:maxDescription], "") + "…"
		}
		wire := wireName(e.name, info.Name, taken)
		e.tools = append(e.tools, &mcpTool{srv: e, wire: wire, raw: info.Name, info: info, def: tool.ToolDefinition{
			Name: wire, Description: "[MCP server " + e.name + "] " + desc, Parameters: schema,
		}})
	}
	sort.Slice(e.tools, func(i, j int) bool { return e.tools[i].wire < e.tools[j].wire })
	if e.info.Resources {
		var list strings.Builder
		for i, r := range res {
			if i >= 40 {
				fmt.Fprintf(&list, "\n… and %d more", len(res)-i)
				break
			}
			fmt.Fprintf(&list, "\n- %s (%s)", r.URI, r.Name)
		}
		desc := "[MCP server " + e.name + "] Read a resource this server offers by its URI."
		if list.Len() > 0 {
			desc += " Resources:" + list.String()
		}
		if len(desc) > maxDescription*2 {
			desc = strings.ToValidUTF8(desc[:maxDescription*2], "") + "…"
		}
		wire := wireName(e.name, resourceToolName, taken)
		e.resTool = &resourceTool{srv: e, wire: wire, def: tool.ToolDefinition{Name: wire, Description: desc, Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"uri": map[string]any{"type": "string", "description": "The resource's URI"}}, "required": []string{"uri"},
		}}}
	}
}

// watch notices a server that went away on its own.
func (m *Manager) watch(e *entry, c *Client) {
	<-c.Done()
	m.mu.Lock()
	if m.closed || e.client != c {
		m.mu.Unlock()
		return
	}
	e.state, e.client, e.tools, e.resTool = StateFailed, nil, nil, nil
	e.err = "the server went away"
	if d := c.Diag(); d != "" {
		e.diag = d
		e.err += ": " + firstLine(d)
	}
	e.retryAt = time.Now().Add(2 * time.Second)
	m.mu.Unlock()
	m.notify()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// relist refreshes a ready server's tools.
func (m *Manager) relist(ctx context.Context, e *entry) {
	m.mu.Lock()
	c := e.client
	m.mu.Unlock()
	if c == nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	infos, err := c.ListTools(lctx)
	m.mu.Lock()
	if err == nil && e.client == c {
		m.setTools(e, infos, e.resources)
		e.stale, e.listedAt = false, time.Now()
	}
	m.mu.Unlock()
	if err == nil {
		m.notify()
	}
}

// Tools is what the servers offer now, in a stable order (a changing order
// would break the model's prompt cache).
func (m *Manager) Tools() []tool.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.entries))
	for n, e := range m.entries {
		if e.state == StateReady {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []tool.Tool
	for _, n := range names {
		e := m.entries[n]
		for _, t := range e.tools {
			out = append(out, t)
		}
		if e.resTool != nil {
			out = append(out, e.resTool)
		}
	}
	return out
}

// ready returns e's live client, starting the server if need be.
func (m *Manager) ready(ctx context.Context, e *entry) (*Client, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("the MCP servers are shutting down")
		}
		switch e.state {
		case StateReady:
			c := e.client
			m.mu.Unlock()
			return c, nil
		case StateNeedsAuth:
			m.mu.Unlock()
			return nil, fmt.Errorf("MCP server %s needs sign-in: open Settings → MCP servers", e.name)
		case StateDisabled, StatePending:
			m.mu.Unlock()
			return nil, fmt.Errorf("MCP server %s is not enabled", e.name)
		}
		settled := m.startLocked(e)
		m.mu.Unlock()
		select {
		case <-settled:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, serverErr(e, fmt.Sprintf("MCP server %s isn't available: %s", e.name, e.err))
}

func (m *Manager) callTimeout(e *entry) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.cfg.TimeoutMs > 0 {
		return time.Duration(e.cfg.TimeoutMs) * time.Millisecond
	}
	return DefaultCallTimeout * time.Millisecond
}

// callFailed turns a failed call into the error the model sees, and updates the
// server's state when the failure says something about it.
func (m *Manager) callFailed(e *entry, c *Client, err error, turn, call context.Context) error {
	if turn.Err() != nil {
		return turn.Err()
	}
	if call.Err() != nil {
		return fmt.Errorf("MCP server %s: the call timed out", e.name)
	}
	var auth *errNeedsAuth
	var rerr *rpcError
	switch {
	case errors.As(err, &auth):
		m.mu.Lock()
		if e.client == c {
			e.state, e.err, e.challenge, e.client, e.tools, e.resTool = StateNeedsAuth, "this server needs sign-in", auth, nil, nil, nil
		}
		m.mu.Unlock()
		m.notify()
		c.Close()
		return fmt.Errorf("MCP server %s needs sign-in: open Settings → MCP servers", e.name)
	case errors.As(err, &rerr):
		return serverErr(e, fmt.Sprintf("MCP server %s: %s", e.name, rerr.Message))
	case errors.Is(err, errInputRequired):
		return fmt.Errorf("MCP server %s: %w", e.name, err)
	}
	select {
	case <-c.Done():
		return serverErr(e, fmt.Sprintf("MCP server %s went away: %s", e.name, err))
	default:
	}
	return serverErr(e, fmt.Sprintf("MCP server %s: %s", e.name, err))
}

// serverErr is an error whose text the server wrote (its messages, its stderr),
// so it reaches the model framed as untrusted data, like a tool's output does.
func serverErr(e *entry, msg string) error {
	return errors.New(tool.WrapUntrusted("the MCP server "+e.name, msg))
}

// Close stops every server.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var stop []*Client
	for _, e := range m.entries {
		if e.client != nil {
			stop = append(stop, e.client)
		}
	}
	m.mu.Unlock()
	closeAll(stop)
}

// --- what the settings UI drives ---

// Connect starts a server that isn't running, without waiting.
func (m *Manager) Connect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stop := m.reload()
	defer closeAll(stop)
	e := m.entries[name]
	if e == nil {
		return fmt.Errorf("no server named %s", name)
	}
	if !e.startable() {
		return fmt.Errorf("server %s can't be started: %s", name, m.whyNot(e))
	}
	if e.state == StateReady || e.state == StateConnecting {
		return nil
	}
	e.retryAt = time.Time{}
	m.startLocked(e)
	return nil
}

func (m *Manager) whyNot(e *entry) string {
	switch {
	case e.expandErr != "":
		return e.expandErr
	case e.state == StatePending:
		return "it hasn't been approved"
	}
	return "it is switched off"
}

// ConnectAll starts every server that can run and isn't, without waiting.
func (m *Manager) ConnectAll() {
	m.mu.Lock()
	stop := m.reload()
	for _, e := range m.entries {
		if e.startable() && (e.state == StateIdle || (e.state == StateFailed && time.Now().After(e.retryAt))) {
			m.startLocked(e)
		}
	}
	m.mu.Unlock()
	closeAll(stop)
}

// Reconnect restarts a server, whatever state it is in.
func (m *Manager) Reconnect(name string) error {
	m.mu.Lock()
	stop := m.reload()
	e := m.entries[name]
	if e == nil {
		m.mu.Unlock()
		closeAll(stop)
		return fmt.Errorf("no server named %s", name)
	}
	if !e.startable() {
		why := m.whyNot(e)
		m.mu.Unlock()
		closeAll(stop)
		return fmt.Errorf("server %s can't be started: %s", name, why)
	}
	if e.client != nil {
		stop = append(stop, e.client)
		e.client = nil
	}
	e.failures, e.retryAt, e.settled = 0, time.Time{}, nil
	e.state = StateIdle
	m.startLocked(e)
	m.mu.Unlock()
	closeAll(stop)
	return nil
}

// Redacted stands in for a secret value on the wire.
const Redacted = "••••••"

// redact hides what a server's config keeps secret. A value that is only a
// ${VAR} reference holds no secret and is shown as is.
func redact(cfg ServerConfig) ServerConfig {
	hide := func(in map[string]string) map[string]string {
		if in == nil {
			return nil
		}
		out := make(map[string]string, len(in))
		for k, v := range in {
			if v == "" || (varRe.MatchString(v) && varRe.ReplaceAllString(v, "") == "") {
				out[k] = v
			} else {
				out[k] = Redacted
			}
		}
		return out
	}
	cfg.Env, cfg.Headers = hide(cfg.Env), hide(cfg.Headers)
	if cfg.OAuth != nil {
		o := *cfg.OAuth
		if o.ClientSecret != "" {
			o.ClientSecret = Redacted
		}
		cfg.OAuth = &o
	}
	return cfg
}

// restoreSecrets fills in the values a client sent back as Redacted from what
// is stored, so editing a server doesn't force retyping its token.
func restoreSecrets(in, prev ServerConfig) (ServerConfig, error) {
	fix := func(kind string, in, prev map[string]string) error {
		for k, v := range in {
			if v != Redacted {
				continue
			}
			old, ok := prev[k]
			if !ok {
				return fmt.Errorf("%s %s has no stored value to keep: type it again", kind, k)
			}
			in[k] = old
		}
		return nil
	}
	if err := fix("environment variable", in.Env, prev.Env); err != nil {
		return in, err
	}
	if err := fix("header", in.Headers, prev.Headers); err != nil {
		return in, err
	}
	if in.OAuth != nil && in.OAuth.ClientSecret == Redacted {
		if prev.OAuth == nil || prev.OAuth.ClientSecret == "" {
			return in, errors.New("the client secret has no stored value to keep: type it again")
		}
		o := *in.OAuth
		o.ClientSecret = prev.OAuth.ClientSecret
		in.OAuth = &o
	}
	return in, nil
}

// Save adds or edits one of the user's servers, then starts it so the user
// sees at once whether it works. replacing is the name being edited ("" for a new one).
func (m *Manager) Save(name, replacing string, cfg ServerConfig) error {
	var prev ServerConfig
	if replacing != "" {
		prev = m.store.Servers()[replacing]
	}
	cfg, err := restoreSecrets(cfg, prev)
	if err != nil {
		return err
	}
	if err := m.store.Save(name, replacing, cfg); err != nil {
		return err
	}
	m.mu.Lock()
	stop := m.reload()
	if e := m.entries[name]; e != nil && e.startable() {
		m.startLocked(e)
	}
	m.mu.Unlock()
	closeAll(stop)
	m.notify()
	return nil
}

// Remove deletes one of the user's servers.
func (m *Manager) Remove(name string) error {
	if err := m.store.Remove(name); err != nil {
		return err
	}
	m.mu.Lock()
	stop := m.reload()
	m.mu.Unlock()
	closeAll(stop)
	m.notify()
	return nil
}

// SetEnabled switches a user server on or off.
func (m *Manager) SetEnabled(name string, enabled bool) error {
	cfg, ok := m.store.Servers()[name]
	if !ok {
		return fmt.Errorf("no server named %s of yours", name)
	}
	cfg.Disabled = !enabled
	return m.Save(name, name, cfg)
}

// SetToolEnabled switches one tool of a user server on or off.
func (m *Manager) SetToolEnabled(name, rawTool string, enabled bool) error {
	cfg, ok := m.store.Servers()[name]
	if !ok {
		return fmt.Errorf("no server named %s of yours", name)
	}
	var keep []string
	for _, t := range cfg.DisabledTools {
		if t != rawTool {
			keep = append(keep, t)
		}
	}
	if !enabled {
		keep = append(keep, rawTool)
	}
	sort.Strings(keep)
	cfg.DisabledTools = keep
	if err := m.store.Save(name, name, cfg); err != nil {
		return err
	}
	m.mu.Lock()
	stop := m.reload()
	m.mu.Unlock()
	closeAll(stop)
	m.notify()
	return nil
}

// Approve records the user's decision on a project's server.
func (m *Manager) Approve(name string, approved bool) error {
	m.mu.Lock()
	stop := m.reload()
	e := m.entries[name]
	if e == nil || e.scope != ScopeProject {
		m.mu.Unlock()
		closeAll(stop)
		return fmt.Errorf("no project server named %s", name)
	}
	hash := e.hash
	m.mu.Unlock()
	closeAll(stop)
	if err := m.store.SetApproval(m.root, name, hash, approved); err != nil {
		return err
	}
	m.mu.Lock()
	stop = m.reload()
	if e := m.entries[name]; e != nil && approved && e.startable() {
		m.startLocked(e)
	}
	m.mu.Unlock()
	closeAll(stop)
	m.notify()
	return nil
}

// --- sign-in ---

// serverURL is the entry's remote address, expanded.
func (m *Manager) remote(name string) (*entry, ServerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[name]
	if e == nil {
		return nil, ServerConfig{}, fmt.Errorf("no server named %s", name)
	}
	if e.cfg.EffectiveType() == TypeStdio {
		return nil, ServerConfig{}, errors.New("a local server has nothing to sign in to")
	}
	cfg, err := e.cfg.Expanded(m.envFor(e))
	return e, cfg, err
}

// BeginAuth starts a sign-in; redirect is a loopback address that will receive
// the browser's answer. It returns the sign-in's id and the page to open.
func (m *Manager) BeginAuth(ctx context.Context, name, redirect string) (id, authURL string, err error) {
	if m.flow == nil {
		return "", "", errors.New("sign-in isn't available")
	}
	e, cfg, err := m.remote(name)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	ch := e.challenge
	m.mu.Unlock()
	p, authURL, err := m.flow.Begin(ctx, cfg, ch, redirect)
	if err != nil {
		return "", "", err
	}
	return p.ID, authURL, nil
}

// CompleteAuth finishes a sign-in with the browser's answer and reconnects the
// servers that use that address. It returns the name of the server that was
// signed in to.
func (m *Manager) CompleteAuth(ctx context.Context, id, code, state, iss string) (string, error) {
	if m.flow == nil {
		return "", errors.New("sign-in isn't available")
	}
	p, ok := m.flow.Take(id)
	if !ok {
		return "", errors.New("that sign-in is unknown or has expired")
	}
	if err := m.flow.Complete(ctx, p, code, state, iss); err != nil {
		return "", err
	}
	name := ""
	m.mu.Lock()
	var names []string
	for n, e := range m.entries {
		if cfg, err := e.cfg.Expanded(m.envFor(e)); err == nil && e.cfg.EffectiveType() != TypeStdio && authKey(cfg.URL) == authKey(p.ServerURL) {
			names = append(names, n)
		}
	}
	m.mu.Unlock()
	for _, n := range names {
		name = n
		_ = m.Reconnect(n)
	}
	return name, nil
}

// SignOut forgets a server's tokens and restarts it.
func (m *Manager) SignOut(name string) error {
	_, cfg, err := m.remote(name)
	if err != nil {
		return err
	}
	if err := m.auth.Forget(cfg.URL); err != nil {
		return err
	}
	return m.Reconnect(name)
}

// --- status ---

// ToolStatus is one tool as the settings page lists it.
type ToolStatus struct {
	Name        string `json:"name"`    // what the model sees
	Raw         string `json:"rawName"` // the server's own name
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	Destructive bool   `json:"destructive,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// ServerStatus is one server as the settings page shows it. Secrets in the
// config are hidden.
type ServerStatus struct {
	Name      string       `json:"name"`
	Scope     Scope        `json:"scope"`
	Config    ServerConfig `json:"config"`
	State     State        `json:"state"`
	Error     string       `json:"error,omitempty"`
	Stderr    string       `json:"stderr,omitempty"`
	Approval  string       `json:"approval,omitempty"`
	Info      *ServerInfo  `json:"info,omitempty"`
	Tools     []ToolStatus `json:"tools,omitempty"`
	Truncated int          `json:"truncated,omitempty"`
	SignedIn  bool         `json:"signedIn,omitempty"`
	Remote    bool         `json:"remote,omitempty"`
	Resources bool         `json:"resources,omitempty"`
}

// Status lists every server of the project, user's first.
func (m *Manager) Status() (servers []ServerStatus, problems []string) {
	m.mu.Lock()
	stop := m.reload()
	for _, e := range m.entries {
		st := ServerStatus{Name: e.name, Scope: e.scope, Config: redact(e.cfg), State: e.state, Error: e.err, Stderr: e.diag,
			Approval: e.approval, Truncated: e.truncated, Remote: e.cfg.EffectiveType() != TypeStdio}
		if e.expandErr != "" && st.Error == "" {
			st.Error = e.expandErr
		}
		if e.state == StateReady || e.info.Name != "" {
			info := e.info
			st.Info = &info
		}
		st.Resources = e.resTool != nil
		if st.Remote && m.auth != nil {
			if cfg, err := e.cfg.Expanded(m.envFor(e)); err == nil {
				st.SignedIn = m.auth.SignedIn(cfg.URL)
			}
		}
		off := map[string]bool{}
		for _, n := range e.cfg.DisabledTools {
			off[n] = true
		}
		wires := map[string]string{}
		for _, t := range e.tools {
			wires[t.raw] = t.wire
		}
		for _, ti := range e.toolInfos {
			ts := ToolStatus{Name: wires[ti.Name], Raw: ti.Name, Title: shorten(ti.Title, 200), Description: shorten(ti.Description, 240), Enabled: !off[ti.Name]}
			if a := ti.Annotations; a != nil {
				ts.ReadOnly = a.ReadOnlyHint != nil && *a.ReadOnlyHint
				ts.Destructive = a.DestructiveHint != nil && *a.DestructiveHint
			}
			st.Tools = append(st.Tools, ts)
		}
		servers = append(servers, st)
	}
	problems = append([]string(nil), m.problems...)
	m.mu.Unlock()
	closeAll(stop)
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Scope != servers[j].Scope {
			return servers[i].Scope == ScopeUser
		}
		return servers[i].Name < servers[j].Name
	})
	return servers, problems
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// ForAgents is the user's servers that are meant for the coding-agent CLIs,
// with variables expanded. Project servers are left to the agents' own readers
// of .mcp.json.
func (m *Manager) ForAgents() map[string]ServerConfig {
	out := map[string]ServerConfig{}
	for name, cfg := range m.store.Servers() {
		if !cfg.Agents || cfg.Disabled {
			continue
		}
		if ex, err := cfg.Expanded(m.getenv); err == nil {
			out[name] = ex
		}
	}
	return out
}

// AgentServers is ForAgents in the form the CLI drivers take, in a stable
// order. A remote server that signs in with OAuth is left out: the agent would
// need the token, and it would outlive the one we could hand over.
func (m *Manager) AgentServers() []tool.AgentServer {
	shared := m.ForAgents()
	names := make([]string, 0, len(shared))
	for n := range shared {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []tool.AgentServer
	for _, n := range names {
		c := shared[n]
		if c.EffectiveType() != TypeStdio && c.OAuth != nil {
			continue
		}
		if c.EffectiveType() != TypeStdio && m.auth != nil && m.auth.SignedIn(c.URL) {
			continue
		}
		out = append(out, tool.AgentServer{Name: n, Type: c.EffectiveType(), Command: c.Command, Args: c.Args, Env: c.Env, URL: c.URL, Headers: c.Headers})
	}
	return out
}

// envFor is the environment ${VAR} in e's config may read. A project's .mcp.json is repository
// content, so it can't name the daemon's own secrets: a server that gets approved shouldn't be
// able to send the master key or a provider key to whoever wrote the file.
// ponytail: a denylist of the daemon's known secrets; arbitrary variables (AWS_*, GITHUB_TOKEN)
// still expand. A per-server allowlist in the approval UI would close that.
func (m *Manager) envFor(e *entry) func(string) (string, bool) {
	if e.scope != ScopeProject {
		return m.getenv
	}
	return func(k string) (string, bool) {
		switch k {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "OPENAI_BASE_URL":
			return "", false
		}
		if strings.HasPrefix(k, "YOAKELAND_") || strings.HasPrefix(k, "YOAKE_") {
			return "", false
		}
		return m.getenv(k)
	}
}
