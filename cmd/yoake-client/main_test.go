package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeStripsControlSequences(t *testing.T) {
	in := "ok\x1b[2J\x1b]52;c;SGk=\x07\rX\n\tY\u009b"
	want := "ok[2J]52;c;SGk=X\n\tY"
	if got := sanitize(in); got != want {
		t.Fatalf("sanitize() = %q, want %q", got, want)
	}
}

func TestParsePermissionAnswerIsExact(t *testing.T) {
	cases := map[string]struct{ allowed, remember, ok bool }{
		"y":                 {true, false, true},
		"yes":               {true, false, true},
		"yr":                {true, true, true},
		"always":            {true, true, true},
		"n":                 {false, false, true},
		"no":                {false, false, true},
		"nr":                {false, true, true},
		"yo":                {},
		"yikes":             {},
		"no, that is wrong": {},
	}
	for in, want := range cases {
		allowed, remember, ok := parsePermissionAnswer(in)
		if allowed != want.allowed || remember != want.remember || ok != want.ok {
			t.Errorf("parsePermissionAnswer(%q) = (%v, %v, %v), want (%v, %v, %v)",
				in, allowed, remember, ok, want.allowed, want.remember, want.ok)
		}
	}
}

func TestBackendPathFindsBinaryOnPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "yoakeland")
	if err := os.WriteFile(want, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := backendPath("yoakeland"); got != want {
		t.Fatalf("backendPath() = %q, want %q", got, want)
	}
}

func TestBackendPathIgnoresWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "yoakeland"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("PATH", t.TempDir())
	// A bare name not on PATH stays bare, so exec fails instead of running the repo's copy.
	if got := backendPath("yoakeland"); got != "yoakeland" {
		t.Fatalf("backendPath() = %q, want the bare name", got)
	}
}
