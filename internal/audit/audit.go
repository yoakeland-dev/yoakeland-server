// Package audit is an append-only record of the security-relevant things a
// yoakeland server did or allowed: who approved or refused what, what ran,
// which settings and credentials changed, who connected. One JSONL file under
// <state-dir>/audit/, rotated by size. It is a trail for the owner to read
// back, not a tamper-proof ledger: there is no hash chain, and anyone who can
// write the state dir can edit it.
//
// Nothing secret goes in. Callers name what changed, never the value, and
// Append additionally scrubs the usual credential shapes from free text (a
// bash command can carry a token) and drops detail keys named like one.
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Event kinds. The list is for filters and the UI; Append does not check it.
const (
	PermDecision    = "perm_decision"    // a person answered a permission ask
	PermAuto        = "perm_auto"        // a rule, a mode or autonomous mode answered it
	ToolExec        = "tool_exec"        // a tool that can change things ran
	ModeChange      = "mode_change"      // a session's mode changed
	RuleAdd         = "rule_add"         // a permission rule was stored
	RuleRemove      = "rule_remove"      // a permission rule was deleted
	LimitPause      = "limit_pause"      // a turn paused at a soft limit / the answer
	ProviderAdd     = "provider_add"     // a provider was registered
	ProviderRemove  = "provider_remove"  // a provider was removed
	SettingsChange  = "settings_change"  // runtime settings, default provider, web tools
	RemoteAccess    = "remote_access"    // the TCP listener was switched or reconfigured
	RemotePassword  = "remote_password"  // the shared TCP secret was replaced
	TokenAdd        = "token_add"        // a per-person token was minted
	TokenRemove     = "token_remove"     // a per-person token was revoked
	PeerScope       = "peer_scope"       // a device was promoted to full access or demoted
	PeerJoin        = "peer_join"        // a device connected
	PeerLeave       = "peer_leave"       // a device disconnected
	PeerKick        = "peer_kick"        // a device was disconnected by another
	AuthFail        = "auth_fail"        // a TCP client presented a bad credential
	SnapshotRestore = "snapshot_restore" // files were rolled back to a snapshot
	TerminalOpen    = "terminal_open"    // a shell was opened
	FileWrite       = "file_write"       // a file was saved from the editor
	FsOp            = "fs_op"            // rename / delete / create from the explorer
	SessionDelete   = "session_delete"   // a chat was permanently deleted
	ProjectRemove   = "project_remove"   // a project was forgotten
	MCPServer       = "mcp_server"       // an MCP server was added, changed, removed, approved, signed in to
	SkillChange     = "skill_change"     // a skill was switched on or off
	ServerRestart   = "server_restart"   // the daemon was asked to restart
)

// Kinds lists every kind, in the order a UI shows them.
var Kinds = []string{
	PermDecision, PermAuto, ToolExec, ModeChange, RuleAdd, RuleRemove, LimitPause,
	ProviderAdd, ProviderRemove, SettingsChange, RemoteAccess, RemotePassword, TokenAdd, TokenRemove, PeerScope,
	PeerJoin, PeerLeave, PeerKick, AuthFail, SnapshotRestore, TerminalOpen, FileWrite, FsOp, SessionDelete, ProjectRemove,
	MCPServer, SkillChange, ServerRestart,
}

// Event is one line of the log.
type Event struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Actor is the device that did it: its name at the time. "local" for a
	// stdio connection, "server" for something the server did by itself.
	Actor   string         `json:"actor,omitempty"`
	IP      string         `json:"ip,omitempty"`
	Network string         `json:"network,omitempty"` // unix | tcp | stdio
	Project string         `json:"project,omitempty"`
	Session string         `json:"session,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	Target  string         `json:"target,omitempty"`  // what it acted on: a command, a path, a name
	Outcome string         `json:"outcome,omitempty"` // allow | deny | ok | error | run | ...
	Detail  map[string]any `json:"detail,omitempty"`
}

const (
	// DefaultMaxBytes is the size at which the current file is rotated.
	DefaultMaxBytes = 5 << 20
	// Keep is how many files exist at most: audit.jsonl plus Keep-1 rotated.
	Keep = 3

	fileName  = "audit.jsonl"
	maxFieldN = 300 // runes kept of Target and of any string in Detail
	// MaxLimit bounds one Read.
	MaxLimit = 500
)

// Log is the append-only file. A nil *Log is a no-op, so callers (and tests'
// bare dependencies) never check.
type Log struct {
	mu       sync.Mutex
	dir      string
	f        *os.File
	size     int64
	seq      int64
	maxBytes int64
	// Warn, if set, hears about write failures (the log keeps going without them).
	Warn func(format string, args ...any)
}

// Open creates dir (0700) and opens audit.jsonl in it for append (0600),
// continuing the sequence numbers where the last run stopped.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700) // a directory made earlier, more openly
	l := &Log{dir: dir, maxBytes: DefaultMaxBytes}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	l.seq = lastSeq(l.path(0))
	if l.seq == 0 {
		l.seq = lastSeq(l.path(1)) // just rotated, or the current file is empty
	}
	return l, nil
}

func (l *Log) path(n int) string {
	if n == 0 {
		return filepath.Join(l.dir, fileName)
	}
	return filepath.Join(l.dir, fmt.Sprintf("audit.%d.jsonl", n))
}

func (l *Log) openFile() error {
	f, err := os.OpenFile(l.path(0), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, info.Size()
	return nil
}

// lastSeq is the sequence number of the last complete line of path, 0 if none.
func lastSeq(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0
	}
	n := min(info.Size(), 64<<10)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, info.Size()-n); err != nil {
		return 0
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var e Event
		if json.Unmarshal(lines[i], &e) == nil && e.Seq > 0 {
			return e.Seq
		}
	}
	return 0
}

// Append stamps e with the next sequence number and the time, scrubs it, and
// writes it. Failure is reported through Warn and otherwise ignored: an audit
// disk problem must not stop the server from working.
func (l *Log) Append(e Event) {
	if l == nil {
		return
	}
	e.Target = clip(Redact(e.Target))
	e.Detail = scrubDetail(e.Detail)
	e.At = time.Now().UTC()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	e.Seq = l.seq + 1
	line, err := json.Marshal(e)
	if err != nil {
		l.warn("audit: %v", err)
		return
	}
	line = append(line, '\n')
	if l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
		if l.f == nil {
			return
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		l.warn("audit: write %s: %v", l.path(0), err)
		return
	}
	l.seq = e.Seq
}

func (l *Log) warn(format string, args ...any) {
	if l.Warn != nil {
		l.Warn(format, args...)
	}
}

// rotateLocked moves audit.jsonl to audit.1.jsonl (and that to audit.2.jsonl,
// and so on), dropping the oldest, then starts a new current file.
func (l *Log) rotateLocked() {
	_ = l.f.Close()
	l.f = nil
	_ = os.Remove(l.path(Keep - 1))
	for i := Keep - 2; i >= 0; i-- {
		if err := os.Rename(l.path(i), l.path(i+1)); err != nil && !os.IsNotExist(err) {
			l.warn("audit: rotate %s: %v", l.path(i), err)
		}
	}
	if err := l.openFile(); err != nil {
		l.warn("audit: reopen after rotate: %v", err)
	}
}

// Close closes the file. Append after Close does nothing.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Query selects events for Read. Zero values match everything.
type Query struct {
	Before  int64 // only events with a lower seq: the page after the last one seen
	Limit   int   // default 100, at most MaxLimit
	Kinds   []string
	Session string
	Project string
	Since   time.Time
}

func (q Query) match(e *Event) bool {
	return (q.Before == 0 || e.Seq < q.Before) &&
		(len(q.Kinds) == 0 || slices.Contains(q.Kinds, e.Kind)) &&
		(q.Session == "" || e.Session == q.Session) &&
		(q.Project == "" || e.Project == q.Project) &&
		(q.Since.IsZero() || !e.At.Before(q.Since))
}

// Read returns matching events newest first, and whether older matches remain
// (ask again with Before set to the last returned Seq).
func (l *Log) Read(q Query) (events []Event, more bool, err error) {
	events = []Event{}
	if l == nil {
		return events, false, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, MaxLimit)

	l.mu.Lock()
	defer l.mu.Unlock()
	for n := 0; n < Keep; n++ {
		data, rerr := os.ReadFile(l.path(n))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			return events, false, rerr
		}
		lines := bytes.Split(data, []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			var e Event
			if len(lines[i]) == 0 || json.Unmarshal(lines[i], &e) != nil || !q.match(&e) {
				continue
			}
			if len(events) == limit {
				return events, true, nil
			}
			events = append(events, e)
		}
	}
	return events, false, nil
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxFieldN {
		return s
	}
	r := []rune(s)
	return string(r[:maxFieldN]) + "…"
}

// secretKey names Detail keys that must never carry a value into the log.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "passwd", "secret", "apikey", "api_key", "authorization", "token"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func scrubDetail(d map[string]any) map[string]any {
	if len(d) == 0 {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		if secretKey(k) {
			continue
		}
		switch x := v.(type) {
		case string:
			out[k] = clip(Redact(x))
		case []string:
			c := make([]string, len(x))
			for i, s := range x {
				c[i] = clip(Redact(s))
			}
			out[k] = c
		default:
			out[k] = v
		}
	}
	return out
}

var redactors = []struct {
	re   *regexp.Regexp
	with string
}{
	// Authorization: Bearer xyz / Basic xyz
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [redacted]"},
	// password=hunter2, "token": "abc", API_KEY: abc
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|passw(?:or)?d|pwd)["']?\s*[:=]\s*["']?)[^\s"'&,;]+`), "$1[redacted]"},
	// --password hunter2, --token=abc, -p is too ambiguous to catch
	{regexp.MustCompile(`(?i)(--?(?:password|passwd|token|api-?key|secret)(?:\s+|=))[^\s]+`), "$1[redacted]"},
	// https://user:pass@host
	{regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`), "$1[redacted]@"},
	// well-known key shapes
	{regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,})\b`), "[redacted]"},
}

// Redact scrubs the usual credential shapes from free text. Best effort: it
// is a safety net for a command line that carries a secret, not a guarantee.
func Redact(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}
