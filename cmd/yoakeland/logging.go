package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

// useColor is decided once at startup: only when stderr is an actual
// terminal and the user hasn't opted out via NO_COLOR (https://no-color.org).
var useColor = term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("NO_COLOR") == ""

const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[36m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31;1m"
)

// logMu also guards logFile: it's written from every connection's own
// goroutine (accept loop, per-peer handler, turn goroutines), so both the
// file handle and interleaved stderr writes need one lock.
var (
	logMu   sync.Mutex
	logFile io.Writer // nil until initLogging opens it — stderr-only until then
)

// logMaxBytes is the size at which yoakeland.log is rotated. A var for tests.
var logMaxBytes int64 = 10 << 20

// rotatingLog is the file behind logFile. It has no lock of its own: every
// call is made under logMu.
type rotatingLog struct {
	path string
	f    *os.File
	size int64
}

func (r *rotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

// rotate moves the current file to <path>.1, replacing the previous copy, and
// starts a new one. Only one old copy is kept, so an unattended daemon can't
// fill the state disk.
func (r *rotatingLog) rotate() error {
	_ = r.f.Close()
	_ = os.Rename(r.path, r.path+".1")
	return r.open()
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	if r.size > 0 && r.size+int64(len(p)) > logMaxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// stdlogWriter takes the stdlib logger's output (what internal/ packages
// write) and sends it through logLine, so it reaches the log file too.
type stdlogWriter struct{}

func (stdlogWriter) Write(p []byte) (int, error) {
	logInfo("%s", strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// logSafe replaces control characters in text a stranger sent (an error
// string, a name), so it can't forge extra lines in the log.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// initLogging opens <stateDir>/logs/yoakeland.log for append and tees every
// subsequent log line to it alongside stderr. A daemon has no attached
// terminal to scroll back through, so this is the only durable record once
// stderr is gone. Best-effort: a failure to open the file (e.g. read-only
// state dir) just falls back to stderr-only rather than blocking startup.
func initLogging(stateDir string) {
	log.SetFlags(0)
	log.SetOutput(stdlogWriter{})

	dir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		logWarn("could not create log directory %s: %v (logging to stderr only)", dir, err)
		return
	}
	r := &rotatingLog{path: filepath.Join(dir, "yoakeland.log")}
	if err := r.open(); err != nil {
		logWarn("could not open log file: %v (logging to stderr only)", err)
		return
	}
	logMu.Lock()
	logFile = r
	logMu.Unlock()
	logInfo("logging to %s", r.path)
}

func logLine(color, level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006/01/02 15:04:05")

	logMu.Lock()
	defer logMu.Unlock()
	if useColor {
		fmt.Fprintf(os.Stderr, "%s %s%-5s%s %s\n", ts, color, level, colorReset, msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s %-5s %s\n", ts, level, msg)
	}
	if logFile != nil {
		// Never colorized — the log file is plain text on disk.
		fmt.Fprintf(logFile, "%s %-5s %s\n", ts, level, msg)
	}
}

func logInfo(format string, args ...any)  { logLine(colorCyan, "INFO", format, args...) }
func logWarn(format string, args ...any)  { logLine(colorYellow, "WARN", format, args...) }
func logError(format string, args ...any) { logLine(colorRed, "ERROR", format, args...) }

// logLineDetail is for the few messages long-winded enough to want a
// different length in each place: short on the terminal, where a person is
// watching in real time, and fully spelled out in the log file, where
// someone reads it later with no other context. Most call sites don't need
// this split and use logInfo/logWarn/logError directly.
func logLineDetail(color, level, short, detail string) {
	ts := time.Now().Format("2006/01/02 15:04:05")

	logMu.Lock()
	defer logMu.Unlock()
	if useColor {
		fmt.Fprintf(os.Stderr, "%s %s%-5s%s %s\n", ts, color, level, colorReset, short)
	} else {
		fmt.Fprintf(os.Stderr, "%s %-5s %s\n", ts, level, short)
	}
	if logFile != nil {
		fmt.Fprintf(logFile, "%s %-5s %s\n", ts, level, detail)
	}
}

func logInfoDetail(short, detail string) { logLineDetail(colorCyan, "INFO", short, detail) }
func logWarnDetail(short, detail string) { logLineDetail(colorYellow, "WARN", short, detail) }

func logFatal(format string, args ...any) {
	logLine(colorRed, "FATAL", format, args...)
	os.Exit(1)
}
