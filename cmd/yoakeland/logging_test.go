package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NET-17: the log file rotates at logMaxBytes, keeping one old copy.
func TestRotatingLogKeepsOneOldCopy(t *testing.T) {
	old := logMaxBytes
	logMaxBytes = 100
	defer func() { logMaxBytes = old }()

	path := filepath.Join(t.TempDir(), "yoakeland.log")
	r := &rotatingLog{path: path}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	defer r.f.Close()
	for _, s := range []string{"a", "b", "c"} {
		if _, err := r.Write([]byte(strings.Repeat(s, 60))); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(path + ".1"); string(b) != strings.Repeat("b", 60) {
		t.Fatalf("yoakeland.log.1 = %q, want the second batch", b)
	}
	if b, _ := os.ReadFile(path); string(b) != strings.Repeat("c", 60) {
		t.Fatalf("yoakeland.log = %q, want the third batch", b)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf("kept more than one old copy: %v", err)
	}
}

// ARC-9: stdlib log output lands in the log file, one entry per line.
func TestStdlogLinesReachLogFile(t *testing.T) {
	var buf bytes.Buffer
	logMu.Lock()
	logFile = &buf
	logMu.Unlock()
	defer func() {
		logMu.Lock()
		logFile = nil
		logMu.Unlock()
	}()
	log.SetFlags(0)
	log.SetOutput(stdlogWriter{})
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	}()

	log.Printf("sandbox: %s", "cannot enforce limits")

	if !strings.Contains(buf.String(), "INFO  sandbox: cannot enforce limits\n") {
		t.Fatalf("stdlib line missing from log file: %q", buf.String())
	}
}

// NET-17: text from a stranger can't add lines to the log.
func TestLogSafeStripsControlCharacters(t *testing.T) {
	got := logSafe("bad\nWARN forged\x1b[31m")
	if strings.ContainsAny(got, "\n\x1b") || got != "bad?WARN forged?[31m" {
		t.Fatalf("logSafe = %q", got)
	}
}
