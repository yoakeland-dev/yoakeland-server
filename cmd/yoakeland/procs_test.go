package main

import (
	"os"
	"testing"
)

func TestReadProcessTreeHasSelf(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	ps := readProcessTree(os.Getpid())
	if len(ps) == 0 || ps[0].PID != os.Getpid() || ps[0].RSSBytes == 0 {
		t.Fatalf("bad tree: %+v", ps)
	}
}
