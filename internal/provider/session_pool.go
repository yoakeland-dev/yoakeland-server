package provider

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"yoakeland/internal/agent"
)

// A CLI agent's process outlives a turn: it is kept per chat so a steer, a
// model switch, an interrupt or a follow-up reaches the agent that is already
// running instead of a fresh process that has to resume it. Providers are
// rebuilt on every set_provider, so the live processes live here, keyed by
// chat, not on the provider.

// nativeSession is one live CLI process serving a chat.
type nativeSession interface {
	// close asks the process to leave (closing its stdin) and kills it if it won't.
	close()
	// dead reports that the process has exited.
	dead() bool
}

type pooled struct {
	sess     nativeSession
	sig      string
	busy     int
	lastUsed time.Time
	// evicted: replaced while a turn was still on it; the last release closes it.
	evicted bool
}

type sessionPool struct {
	mu   sync.Mutex
	live map[string]*pooled
	once sync.Once
}

var nativePool = &sessionPool{live: map[string]*pooled{}}

// Pool limits, set from the runtime settings poolIdleMinutes / poolMaxLive.
var (
	poolIdle    atomic.Int64 // nanoseconds; 0 = default
	poolMaxLive atomic.Int64 // 0 = unlimited
)

const defaultIdleTimeout = 30 * time.Minute

// SetPoolLimits sets how long an unused CLI process lives and how many may
// live at once (0 = no cap). The cap is soft: a turn is never refused, the
// least recently used idle process is evicted to make room, and if every one
// is mid-turn the pool just goes over.
func SetPoolLimits(idle time.Duration, maxLive int) {
	poolIdle.Store(int64(idle))
	poolMaxLive.Store(int64(maxLive))
}

func idleTimeout() time.Duration {
	if d := time.Duration(poolIdle.Load()); d > 0 {
		return d
	}
	return defaultIdleTimeout
}

// PoolStats is what the status payload reports about live CLI processes.
type PoolStats struct {
	Live, Busy, Max int
	IdleTimeoutSec  int
	ByKind          map[string]int
	OldestIdleSec   int
}

func NativePoolStats() PoolStats {
	pl := nativePool
	pl.mu.Lock()
	defer pl.mu.Unlock()
	st := PoolStats{Max: int(poolMaxLive.Load()), IdleTimeoutSec: int(idleTimeout() / time.Second), ByKind: map[string]int{}}
	for k, e := range pl.live {
		st.Live++
		if kind := k[strings.LastIndex(k, "|")+1:]; kind != "" {
			st.ByKind[kind]++
		}
		if e.busy > 0 {
			st.Busy++
		} else if idle := int(time.Since(e.lastUsed) / time.Second); idle > st.OldestIdleSec {
			st.OldestIdleSec = idle
		}
	}
	return st
}

func poolKey(nt *agent.NativeTurn, kind CLIKind) string {
	if nt.SessionKey == "" {
		return ""
	}
	return nt.SessionKey + "|" + string(kind)
}

// acquireSession returns the chat's live session for kind if it was launched
// with the same sig and usable says it can serve this turn, else closes it and
// launches a new one (reused says which). release must be called when the turn ends: it keeps a
// pooled session alive and closes an ephemeral one. A turn with no SessionKey
// gets an ephemeral session, one process per turn.
func acquireSession(nt *agent.NativeTurn, kind CLIKind, sig string, usable func(nativeSession) bool, launch func() (nativeSession, error)) (s nativeSession, reused bool, release func(), err error) {
	key := poolKey(nt, kind)
	if key == "" {
		s, err := launch()
		if err != nil {
			return nil, false, nil, err
		}
		return s, false, s.close, nil
	}
	nativePool.once.Do(func() { go nativePool.reap() })

	pl := nativePool
	pl.mu.Lock()
	old := pl.live[key]
	if old != nil && old.sig == sig && !old.sess.dead() && usable(old.sess) {
		old.busy++
		pl.mu.Unlock()
		return old.sess, true, func() { pl.release(key, old) }, nil
	}
	delete(pl.live, key)
	closeOld := old != nil && old.busy == 0
	if old != nil && !closeOld {
		old.evicted = true // never kill a turn that is mid-flight
	}
	pl.mu.Unlock()
	if closeOld {
		old.sess.close()
	}

	s, err = launch()
	if err != nil {
		return nil, false, nil, err
	}
	pl.mu.Lock()
	// launch ran outside the lock: another acquirer may have won the race.
	if cur := pl.live[key]; cur != nil && cur.sig == sig && !cur.sess.dead() {
		cur.busy++
		pl.mu.Unlock()
		s.close()
		return cur.sess, true, func() { pl.release(key, cur) }, nil
	}
	var evicted []*pooled
	if max := int(poolMaxLive.Load()); max > 0 {
		evicted = pl.evictLRULocked(len(pl.live) + 1 - max)
	}
	e := &pooled{sess: s, sig: sig, busy: 1, lastUsed: time.Now()}
	pl.live[key] = e
	pl.mu.Unlock()
	closeAll(evicted)
	return s, false, func() { pl.release(key, e) }, nil
}

// evictLRULocked removes up to n idle sessions, least recently used first,
// and returns them to be closed after the lock is dropped. Busy sessions are
// never touched.
func (pl *sessionPool) evictLRULocked(n int) []*pooled {
	var out []*pooled
	for ; n > 0; n-- {
		var oldestKey string
		var oldest *pooled
		for k, e := range pl.live {
			if e.busy == 0 && (oldest == nil || e.lastUsed.Before(oldest.lastUsed)) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			break
		}
		delete(pl.live, oldestKey)
		out = append(out, oldest)
	}
	return out
}

func (pl *sessionPool) release(key string, e *pooled) {
	pl.mu.Lock()
	e.busy--
	e.lastUsed = time.Now()
	if e.sess.dead() && pl.live[key] == e {
		delete(pl.live, key)
	}
	closeNow := e.evicted && e.busy == 0
	pl.mu.Unlock()
	if closeNow {
		e.sess.close()
	}
}

// liveSession is the chat's session for kind, or nil, for calls that act on a
// turn already running (a steer).
func liveSession(nt *agent.NativeTurn, kind CLIKind) nativeSession {
	nativePool.mu.Lock()
	defer nativePool.mu.Unlock()
	if e := nativePool.live[poolKey(nt, kind)]; e != nil && !e.sess.dead() {
		return e.sess
	}
	return nil
}

// CloseNativeSessions ends every CLI process a chat has, for when the chat is
// deleted. sessionKey is agent.NativeTurn.SessionKey.
func CloseNativeSessions(sessionKey string) {
	nativePool.closeWhere(func(key string) bool { return strings.HasPrefix(key, sessionKey+"|") })
}

// CloseAllNativeSessions ends every CLI process, for shutdown.
func CloseAllNativeSessions() { nativePool.closeWhere(func(string) bool { return true }) }

func (pl *sessionPool) closeWhere(match func(key string) bool) {
	pl.mu.Lock()
	var doomed []*pooled
	for k, e := range pl.live {
		if match(k) {
			doomed = append(doomed, e)
			delete(pl.live, k)
		}
	}
	pl.mu.Unlock()
	closeAll(doomed)
}

// closeAll closes in parallel: a wedged process takes up to 3s to give up, and
// N of them shouldn't make shutdown or a chat deletion take 3N.
func closeAll(doomed []*pooled) {
	var wg sync.WaitGroup
	for _, e := range doomed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.sess.close()
		}()
	}
	wg.Wait()
}

// reap closes sessions nobody has used for idleTimeout, never one mid-turn.
func (pl *sessionPool) reap() {
	for range time.Tick(time.Minute) {
		pl.closeIdle(idleTimeout())
	}
}

func (pl *sessionPool) closeIdle(after time.Duration) {
	pl.mu.Lock()
	var doomed []*pooled
	for k, e := range pl.live {
		// A crashed idle process is pruned now instead of waiting out the timeout.
		if e.busy == 0 && (e.sess.dead() || time.Since(e.lastUsed) > after) {
			doomed = append(doomed, e)
			delete(pl.live, k)
		}
	}
	pl.mu.Unlock()
	closeAll(doomed)
}
