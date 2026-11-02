//go:build unix

package main

import "testing"

func TestLockServer(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	a, err := lockServer()
	if err != nil || a == nil {
		t.Fatalf("first lock: %v %v", a, err)
	}
	if _, err := lockServer(); err == nil {
		t.Fatal("second server should be refused")
	}
	a.Close()
	if c, err := lockServer(); err != nil || c == nil {
		t.Fatalf("lock should free on close: %v", err)
	}
}
