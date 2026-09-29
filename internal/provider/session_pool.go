package provider

import (
	"strings"
	"sync"
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
}

type sessionPool struct {
	mu   sync.Mutex
	live map[string]*pooled
	once sync.Once
}

var nativePool = &sessionPool{live: map[string]*pooled{}}

const idleTimeout = 30 * time.Minute

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
	pl.mu.Unlock()
	if old != nil {
		old.sess.close()
	}

	s, err = launch()
	if err != nil {
		return nil, false, nil, err
	}
	e := &pooled{sess: s, sig: sig, busy: 1, lastUsed: time.Now()}
	pl.mu.Lock()
	pl.live[key] = e
	pl.mu.Unlock()
	return s, false, func() { pl.release(key, e) }, nil
}

func (pl *sessionPool) release(key string, e *pooled) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	e.busy--
	e.lastUsed = time.Now()
	if e.sess.dead() && pl.live[key] == e {
		delete(pl.live, key)
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
	for _, e := range doomed {
		e.sess.close()
	}
}

// reap closes sessions nobody has used for idleTimeout, never one mid-turn.
func (pl *sessionPool) reap() {
	for range time.Tick(5 * time.Minute) {
		pl.closeIdle(idleTimeout)
	}
}

func (pl *sessionPool) closeIdle(after time.Duration) {
	pl.mu.Lock()
	var doomed []*pooled
	for k, e := range pl.live {
		if e.busy == 0 && time.Since(e.lastUsed) > after {
			doomed = append(doomed, e)
			delete(pl.live, k)
		}
	}
	pl.mu.Unlock()
	for _, e := range doomed {
		e.sess.close()
	}
}
