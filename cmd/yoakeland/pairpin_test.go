package main

import (
	"sync"
	"testing"
	"time"
)

func TestPINFormatting(t *testing.T) {
	for in, want := range map[string]string{"4821-7733": "48217733", "4821 7733": "48217733", " 48217733\n": "48217733", "abc": ""} {
		if got := normalizePIN(in); got != want {
			t.Errorf("normalizePIN(%q) = %q, want %q", in, got, want)
		}
	}
	if got := formatPIN("48217733"); got != "4821-7733" {
		t.Errorf("formatPIN = %q", got)
	}
	r := newPinRegistry()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p := r.Mint()
		if len(p) != pinDigits || normalizePIN(p) != p {
			t.Fatalf("minted %q, want %d digits", p, pinDigits)
		}
		seen[p] = true
	}
	if len(seen) < 45 {
		t.Errorf("50 PINs gave only %d distinct values", len(seen))
	}
}

func TestPINGuessesAreCountedWhenSessionsBegin(t *testing.T) {
	r := newPinRegistry()
	r.Mint()
	var held []*pairPin
	for i := 0; i < pinAttempts; i++ { // all still running: a guesser doesn't get to wait for each answer
		p, err := r.begin()
		if err != nil {
			t.Fatalf("attempt %d refused: %v", i, err)
		}
		held = append(held, p)
	}
	if _, err := r.begin(); err == nil {
		t.Fatal("a sixth parallel session was allowed")
	}
	for _, p := range held {
		r.settle(p)
	}
}

func TestPINCannotBeSpentTwiceOrAfterExpiry(t *testing.T) {
	r := newPinRegistry()
	r.Mint()
	a, _ := r.begin()
	b, _ := r.begin()
	if !r.spend(a) || r.spend(b) {
		t.Fatal("of two correct sessions exactly the first should win")
	}
	r.settle(a)
	r.settle(b)
	if _, err := r.begin(); err == nil {
		t.Fatal("a used PIN accepted a new session")
	}

	now := time.Now()
	r.now = func() time.Time { return now }
	r.Mint()
	c, _ := r.begin()
	now = now.Add(PairingTTL + time.Second)
	if r.spend(c) {
		t.Fatal("spent a PIN after it expired")
	}
	if _, err := r.begin(); err == nil {
		t.Fatal("an expired PIN accepted a new session")
	}
}

func TestPINReplacedOnlyWhenNothingIsStillRunning(t *testing.T) {
	r := newPinRegistry()
	var mu sync.Mutex
	var announced []string
	r.onExhausted = func(p string) { mu.Lock(); announced = append(announced, p); mu.Unlock() }
	first := r.Mint()
	var ps []*pairPin
	for i := 0; i < pinAttempts; i++ {
		p, _ := r.begin()
		ps = append(ps, p)
	}
	// Four fail while the fifth, which might be the right one, is still going: the PIN must live.
	for _, p := range ps[:pinAttempts-1] {
		r.settle(p)
	}
	if len(announced) != 0 {
		t.Fatal("the PIN was replaced under a session still in progress")
	}
	if !r.spend(ps[pinAttempts-1]) {
		t.Fatal("the last session could not spend the PIN")
	}
	r.settle(ps[pinAttempts-1])
	if len(announced) != 0 {
		t.Fatal("a PIN that was used was announced as dead")
	}

	// All five fail: now it is replaced, and the new one is the active one.
	r.Mint()
	ps = nil
	for i := 0; i < pinAttempts; i++ {
		p, _ := r.begin()
		ps = append(ps, p)
	}
	for _, p := range ps {
		r.settle(p)
	}
	if len(announced) != 1 || announced[0] == first {
		t.Fatalf("announced %v", announced)
	}
	if p, err := r.begin(); err != nil || p.value != announced[0] {
		t.Fatalf("the replacement is not the active PIN: %v", err)
	}
}

func TestNilPINRegistryRefuses(t *testing.T) {
	var r *pinRegistry
	if _, err := r.begin(); err == nil {
		t.Fatal("nil registry began a session")
	}
}
