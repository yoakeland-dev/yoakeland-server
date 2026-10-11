package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// A pairing PIN is the short secret a person types into a new device. It is never sent: it seeds a
// CPace exchange (pairpake.go), so what an eavesdropper or an impostor can do is make online
// guesses, and those are counted here. 8 digits with 5 guesses is about 5 in 100 million.
const (
	pinDigits   = 8
	pinAttempts = 5
)

var errNoPIN = errors.New("pairing failed")

type pairPin struct {
	value   string
	expires time.Time
	left    int  // guesses still allowed: every session that begins spends one, parallel ones included
	used    bool // a session proved it knew the PIN; it works once
	running int  // sessions that began and have not settled yet
}

// pinRegistry holds the one PIN that is currently usable. A fresh one replaces it, and a PIN whose
// guesses ran out is replaced by a new one (onExhausted), since running out means someone typed it
// wrong five times or is guessing.
type pinRegistry struct {
	mu          sync.Mutex
	cur         *pairPin
	now         func() time.Time
	ttl         time.Duration
	onExhausted func(newPIN string) // called outside the lock, with the PIN that replaced the dead one
}

func newPinRegistry() *pinRegistry { return &pinRegistry{now: time.Now, ttl: PairingTTL} }

// OnExhausted sets what is told the replacement when a PIN runs out of guesses.
func (r *pinRegistry) OnExhausted(f func(newPIN string)) {
	r.mu.Lock()
	r.onExhausted = f
	r.mu.Unlock()
}

// Mint makes a PIN the active one and returns it as digits.
func (r *pinRegistry) Mint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mintLocked()
}

func (r *pinRegistry) mintLocked() string {
	n, err := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(pinDigits), nil))
	if err != nil {
		panic(err) // no randomness is not a state to carry on from
	}
	r.cur = &pairPin{value: fmt.Sprintf("%0*d", pinDigits, n), expires: r.now().Add(r.ttl), left: pinAttempts}
	return r.cur.value
}

// begin spends one guess and returns the active PIN for a session to run its exchange against. It
// says nothing about why it refused (none active, expired, out of guesses, already used).
func (r *pinRegistry) begin() (*pairPin, error) {
	if r == nil {
		return nil, errNoPIN
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.cur
	if p == nil || p.used || p.left <= 0 || !r.now().Before(p.expires) {
		return nil, errNoPIN
	}
	p.left--
	p.running++
	return p, nil
}

// spend marks the PIN used, once: of two sessions that both prove they know it, only one gets it.
func (r *pinRegistry) spend(p *pairPin) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.used || r.cur != p || !r.now().Before(p.expires) {
		return false
	}
	p.used = true
	return true
}

// settle closes a session's books. A session that spent the PIN is the normal end; once the last
// guess is taken and nothing is still running, a PIN nobody got right is dead, so a new one takes
// its place. (Waiting for the others means a failing session can't kill the PIN under a correct
// one that is still mid-exchange.)
func (r *pinRegistry) settle(p *pairPin) {
	r.mu.Lock()
	var replacement string
	notify := r.onExhausted
	p.running--
	if !p.used && p.left <= 0 && p.running == 0 && r.cur == p {
		replacement = r.mintLocked()
	}
	r.mu.Unlock()
	if replacement != "" && notify != nil {
		notify(replacement)
	}
}

// normalizePIN is what a person typed, reduced to the digits: 4821-7733, "4821 7733" and
// 48217733 are the same PIN.
func normalizePIN(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// formatPIN is digits shown for reading aloud and typing: 4821-7733.
func formatPIN(digits string) string {
	if len(digits) != pinDigits {
		return digits
	}
	return digits[:pinDigits/2] + "-" + digits[pinDigits/2:]
}
