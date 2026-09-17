package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// initLogging opens <stateDir>/logs/yoakeland.log for append and tees every
// subsequent log line to it alongside stderr. A daemon has no attached
// terminal to scroll back through, so this is the only durable record once
// stderr is gone. Best-effort: a failure to open the file (e.g. read-only
// state dir) just falls back to stderr-only rather than blocking startup.
func initLogging(stateDir string) {
	dir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		logWarn("could not create log directory %s: %v (logging to stderr only)", dir, err)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "yoakeland.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		logWarn("could not open log file: %v (logging to stderr only)", err)
		return
	}
	logMu.Lock()
	logFile = f
	logMu.Unlock()
	logInfo("logging to %s", f.Name())
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
