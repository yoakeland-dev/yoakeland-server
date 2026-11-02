//go:build unix

package main

import "testing"

func TestLockProject(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	a, err := lockProject("/p/one")
	if err != nil || a == nil {
		t.Fatalf("first lock: %v %v", a, err)
	}
	if _, err := lockProject("/p/one"); err == nil {
		t.Fatal("second lock on the same project should fail")
	}
	if b, err := lockProject("/p/two"); err != nil || b == nil {
		t.Fatalf("another project should lock: %v", err)
	}
	a.Close()
	if c, err := lockProject("/p/one"); err != nil || c == nil {
		t.Fatalf("lock should free on close: %v", err)
	}
}
