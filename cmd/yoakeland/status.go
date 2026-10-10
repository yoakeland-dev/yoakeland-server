package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/protocol"
	"yoakeland/internal/provider"
)

var processStart = time.Now()

// serverStatus is the snapshot every server-sent ping carries (SPEC.md §2a)
// for a client's status bar.
func serverStatus(deps connDeps) *protocol.ServerStatus {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	st := &protocol.ServerStatus{
		UptimeSec: int64(time.Since(processStart).Seconds()),
		CPUs:      runtime.NumCPU(),
		HeapBytes: mem.HeapAlloc,
		Version:   version,
	}
	if deps.projects != nil {
		st.ActiveTurns, st.OpenProjects = deps.projects.Stats()
	}
	if deps.peers != nil {
		st.Peers = deps.peers.Count()
	}
	st.Load1, st.MemTotalBytes, st.MemAvailBytes = readHostStats()
	if ps := provider.NativePoolStats(); ps.Live > 0 {
		st.NativeSessions = &protocol.NativeSessionsStatus{Live: ps.Live, Busy: ps.Busy, Max: ps.Max,
			IdleTimeoutSec: ps.IdleTimeoutSec, ByKind: ps.ByKind, OldestIdleSec: ps.OldestIdleSec}
	}
	if deps.projects != nil {
		st.QueuedTurns = deps.projects.QueuedTurns()
		if _, limit := deps.projects.limits.Get(); limit > 0 {
			st.Budgets = []protocol.Budget{{Scope: "session", Period: "total", LimitUSD: limit}}
		}
	}
	if deps.status != nil {
		st.RateLimits = deps.status.rateLimits()
		st.Spend = deps.status.ledger.Totals(time.Now())
	}
	return st
}

// statusHub is the server-wide state behind the status bar that isn't a
// process metric: subscription rate limits, the spend ledger, and the
// pushes that keep peers current between heartbeats (pending_asks,
// git_status, and an out-of-band status ping). A nil *statusHub is a no-op,
// so tests' bare connDeps keep working.
type statusHub struct {
	peers  *PeerRegistry
	ledger *spendLedger
	status func() *protocol.ServerStatus // set once deps exists
	asks   func() []protocol.PendingAsk
	// terminals lists every terminal server-wide (ProjectRegistry.Terminals).
	terminals func() []protocol.TerminalSummary

	// probe reads one CLI's subscription usage without a turn; detect says
	// whether its binary is installed. Tests swap both.
	probe  func(context.Context, provider.CLIKind) (*protocol.ProviderRateLimits, error)
	detect func(provider.CLIKind) bool
	kick   chan struct{}

	mu        sync.Mutex
	limits    map[string]protocol.ProviderRateLimits
	probedAt  map[provider.CLIKind]time.Time
	probeErrs map[provider.CLIKind]string
	// lastRead is each CLI's last successful read, so a provider registered
	// (or re-enabled) within usageMinGap of it gets numbers without a probe.
	lastRead map[provider.CLIKind]protocol.ProviderRateLimits
}

func newStatusHub(peers *PeerRegistry, stateDir string) *statusHub {
	return &statusHub{
		peers:     peers,
		ledger:    newSpendLedger(stateDir),
		probe:     provider.ProbeRateLimits,
		detect:    provider.DetectCLI,
		kick:      make(chan struct{}, 1),
		limits:    map[string]protocol.ProviderRateLimits{},
		probedAt:  map[provider.CLIKind]time.Time{},
		probeErrs: map[provider.CLIKind]string{},
		lastRead:  map[provider.CLIKind]protocol.ProviderRateLimits{},
	}
}

// SetRateLimits overlays usage a CLI reported mid-turn onto
// providerName's last known numbers (a Claude rate_limit_event carries one
// window at a time) and pushes the result.
func (h *statusHub) SetRateLimits(providerName string, rl protocol.ProviderRateLimits) {
	if h == nil {
		return
	}
	rl.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	h.mu.Lock()
	h.limits[providerName] = h.limits[providerName].Merge(rl)
	h.mu.Unlock()
	h.PushStatus()
}

// ForgetRateLimits drops a removed provider's numbers.
func (h *statusHub) ForgetRateLimits(providerName string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	_, had := h.limits[providerName]
	delete(h.limits, providerName)
	h.mu.Unlock()
	if had {
		h.PushStatus()
	}
}

const (
	// usageInterval is how often subscription usage is re-read while any
	// client is connected — the same data a CLI's own /usage shows, and
	// reading it costs no quota. Nobody watching, no probes.
	usageInterval = 5 * time.Minute
	// usageMinGap keeps a kick (a client connecting, a provider being
	// registered) from re-probing a CLI that was just read.
	usageMinGap  = time.Minute
	usageTimeout = 30 * time.Second
)

// KickUsage asks the poller for a refresh now rather than at its next tick.
func (h *statusHub) KickUsage() {
	if h == nil {
		return
	}
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// StartUsagePolling reads every registered cli_claude/cli_codex provider's
// subscription usage at startup, then every usageInterval while a client
// is connected and whenever KickUsage is called (SPEC.md §2a rateLimits).
func (h *statusHub) StartUsagePolling(ctx context.Context, store *provider.ProviderStore) {
	go func() {
		h.refreshUsage(ctx, store)
		t := time.NewTicker(usageInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if h.peers.Count() == 0 {
					continue
				}
			case <-h.kick:
			}
			h.refreshUsage(ctx, store)
		}
	}()
}

// refreshUsage probes each CLI once, however many providers point at it
// (they share the one login on this machine), and replaces those
// providers' numbers. A CLI read within usageMinGap isn't probed again, but
// a provider of it that has no numbers (just registered, or disabled and
// re-enabled) still gets that last read. A failed probe keeps the last good
// numbers and is logged once per distinct error, not on every tick.
func (h *statusHub) refreshUsage(ctx context.Context, store *provider.ProviderStore) {
	byKind := map[provider.CLIKind][]string{}
	for _, cfg := range store.List() {
		if kind, ok := provider.CLIKindOf(cfg.Kind); ok {
			byKind[kind] = append(byKind[kind], cfg.Name)
		}
	}
	changed := false
	for kind, names := range byKind {
		h.mu.Lock()
		if time.Since(h.probedAt[kind]) < usageMinGap {
			if last, ok := h.lastRead[kind]; ok {
				for _, name := range names {
					if _, has := h.limits[name]; !has {
						h.limits[name] = last
						changed = true
					}
				}
			}
			h.mu.Unlock()
			continue
		}
		h.mu.Unlock()
		if !h.detect(kind) {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, usageTimeout)
		rl, err := h.probe(pctx, kind)
		cancel()
		if errors.Is(err, provider.ErrUsageUnsupported) || ctx.Err() != nil {
			continue
		}
		h.mu.Lock()
		h.probedAt[kind] = time.Now()
		if err != nil {
			if h.probeErrs[kind] != err.Error() {
				logWarn("reading %s subscription usage: %v", kind, err)
			}
			h.probeErrs[kind] = err.Error()
			h.mu.Unlock()
			continue
		}
		delete(h.probeErrs, kind)
		rl.Kind = "cli_" + string(kind)
		rl.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		h.lastRead[kind] = *rl
		for _, name := range names {
			// Removed while the probe ran: don't bring its entry back.
			if _, still := store.Get(name); still {
				h.limits[name] = *rl
			}
		}
		h.mu.Unlock()
		changed = true
	}
	if changed {
		h.PushStatus()
	}
}

func (h *statusHub) rateLimits() map[string]protocol.ProviderRateLimits {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.limits) == 0 {
		return nil
	}
	out := make(map[string]protocol.ProviderRateLimits, len(h.limits))
	for k, v := range h.limits {
		out[k] = v
	}
	return out
}

// PushStatus sends every peer a status ping now instead of on its next
// heartbeat tick. It's an ordinary ping, so clients pong it as usual.
func (h *statusHub) PushStatus() {
	if h == nil || h.status == nil {
		return
	}
	h.peers.BroadcastEvent("", protocol.EventPing, "", protocol.PingData{Status: h.status()})
}

// TurnCompleted feeds the spend ledger and pushes the new totals.
func (h *statusHub) TurnCompleted(d protocol.TurnCompletedData) {
	if h == nil {
		return
	}
	if d.Usage != nil && d.Usage.CostUSD > 0 {
		h.ledger.Record(time.Now(), d.Usage.CostUSD)
	}
	h.PushStatus()
}

func (h *statusHub) pendingAsks() protocol.PendingAsksData {
	asks := []protocol.PendingAsk{}
	if h.asks != nil {
		asks = append(asks, h.asks()...)
	}
	return protocol.PendingAsksData{Asks: asks}
}

// SendPendingAsks sends the full snapshot to one connection (on connect).
func (h *statusHub) SendPendingAsks(ipc *protocol.IPCStream) {
	if h == nil {
		return
	}
	_ = ipc.SendEvent(protocol.EventPendingAsks, "", "", "", h.pendingAsks())
}

// BroadcastPendingAsks sends the snapshot to every peer (an ask was
// raised, answered, or cancelled).
func (h *statusHub) BroadcastPendingAsks() {
	if h == nil {
		return
	}
	h.peers.BroadcastEvent("", protocol.EventPendingAsks, "", h.pendingAsks())
}

// SendTerminals sends the server-wide terminal_index to one connection (on
// connect).
func (h *statusHub) SendTerminals(ipc *protocol.IPCStream) {
	if h == nil || h.terminals == nil {
		return
	}
	_ = ipc.SendEvent(protocol.EventTerminalIndex, "", "", "", protocol.TerminalIndexData{Terminals: h.terminals()})
}

// BroadcastTerminals pushes terminal_index to every peer (a terminal
// started, exited or closed somewhere).
func (h *statusHub) BroadcastTerminals() {
	if h == nil || h.terminals == nil {
		return
	}
	h.peers.BroadcastEvent("", protocol.EventTerminalIndex, "", protocol.TerminalIndexData{Terminals: h.terminals()})
}

// BroadcastGit pushes a project's changed git state to every peer.
func (h *statusHub) BroadcastGit(st protocol.GitStatusData) {
	if h == nil {
		return
	}
	h.peers.BroadcastEvent("", protocol.EventGitStatus, "", st)
}

// spendLedger is server-wide spend per local calendar day, persisted at
// <-state-dir>/spend.json — the per-project usage.json files only keep
// running totals, which can't answer "today" or "this month". Spend from
// before this ledger existed isn't in it.
// One map entry per day forever (a few KB a decade).
type spendLedger struct {
	mu   sync.Mutex
	path string
	days map[string]float64 // "2006-01-02" (server local time) -> USD
}

func newSpendLedger(stateDir string) *spendLedger {
	l := &spendLedger{path: filepath.Join(stateDir, "spend.json"), days: map[string]float64{}}
	if b, err := os.ReadFile(l.path); err == nil {
		var f struct {
			Days map[string]float64 `json:"days"`
		}
		if json.Unmarshal(b, &f) == nil && f.Days != nil {
			l.days = f.Days
		}
	}
	return l
}

func (l *spendLedger) Record(now time.Time, usd float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.days[now.Local().Format("2006-01-02")] += usd
	b, err := json.MarshalIndent(struct {
		Days map[string]float64 `json:"days"`
	}{l.days}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(l.path+".tmp", b, 0644); err == nil {
		_ = os.Rename(l.path+".tmp", l.path)
	}
}

func (l *spendLedger) Totals(now time.Time) *protocol.ServerSpend {
	l.mu.Lock()
	defer l.mu.Unlock()
	day := now.Local().Format("2006-01-02")
	sp := &protocol.ServerSpend{TodayUSD: l.days[day]}
	for d, usd := range l.days {
		if d[:7] == day[:7] {
			sp.MonthUSD += usd
		}
	}
	return sp
}

// readHostStats reads load and memory from /proc.
// ponytail: Linux only, zeros elsewhere (omitted on the wire); add
// sysctl/GlobalMemoryStatusEx readers if a mac/windows host needs them.
func readHostStats() (load1 float64, memTotal, memAvail uint64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text()) // "MemTotal:  16318132 kB"
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			memTotal = kb * 1024
		case "MemAvailable:":
			memAvail = kb * 1024
		}
	}
	return
}
