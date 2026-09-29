package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Usage the CLI agents record themselves, outside yoakeland: Claude Code's
// transcripts (~/.claude/projects/**/*.jsonl) and Codex's rollouts
// (~/.codex/sessions/**/*.jsonl), the same files ccusage and T3 Code read.
// Sessions yoakeland drove itself are dropped by the caller (their native
// ids) so nothing is counted twice.

type extRec struct {
	hour                      int64
	source                    string // "claude_code" | "codex"
	session, cwd, model, dkey string
	in, cached, cacheWrite    int
	out                       int
	reportedUSD               float64 // < 0: price it
	yoake                     bool    // Codex tags rollouts yoakeland started
}

type extFile struct {
	mod    time.Time
	size   int64
	recs   []extRec
	titles map[string]string
}

var extFiles = struct {
	sync.Mutex
	m map[string]*extFile
}{m: map[string]*extFile{}}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// scanExternal returns every CLI record, plus session titles by session id.
// Files are re-read only when their size or mtime changed.
func scanExternal() ([]extRec, map[string]string) {
	var recs []extRec
	titles := map[string]string{}
	visit := func(root string, parse func(*os.File) *extFile) {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			extFiles.Lock()
			c := extFiles.m[path]
			extFiles.Unlock()
			if c == nil || !c.mod.Equal(fi.ModTime()) || c.size != fi.Size() {
				f, err := os.Open(path)
				if err != nil {
					return nil
				}
				c = parse(f)
				f.Close()
				c.mod, c.size = fi.ModTime(), fi.Size()
				extFiles.Lock()
				extFiles.m[path] = c
				extFiles.Unlock()
			}
			recs = append(recs, c.recs...)
			for k, v := range c.titles {
				titles[k] = v
			}
			return nil
		})
	}
	visit(claudeDir(), parseClaudeTranscript)
	visit(filepath.Join(codexDir(), "sessions"), parseCodexRollout)
	visit(filepath.Join(codexDir(), "archived_sessions"), parseCodexRollout)
	for k, v := range codexThreadNames() {
		titles[k] = v
	}
	return recs, titles
}

func lines(f *os.File, each func([]byte)) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		each(sc.Bytes())
	}
}

func parseClaudeTranscript(f *os.File) *extFile {
	out := &extFile{titles: map[string]string{}}
	lines(f, func(b []byte) {
		// Most lines are tool output; only these carry what's read below.
		if !bytes.Contains(b, []byte(`"usage"`)) && !bytes.Contains(b, []byte(`-title"`)) {
			return
		}
		var l struct {
			Type        string    `json:"type"`
			Timestamp   time.Time `json:"timestamp"`
			SessionID   string    `json:"sessionId"`
			Cwd         string    `json:"cwd"`
			RequestID   string    `json:"requestId"`
			CustomTitle string    `json:"customTitle"`
			AITitle     string    `json:"aiTitle"`
			CostUSD     *float64  `json:"costUSD"`
			Message     *struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					Input      int `json:"input_tokens"`
					CacheWrite int `json:"cache_creation_input_tokens"`
					CacheRead  int `json:"cache_read_input_tokens"`
					Output     int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &l) != nil {
			return
		}
		switch l.Type {
		case "custom-title":
			out.titles[l.SessionID] = l.CustomTitle
		case "ai-title":
			if _, custom := out.titles[l.SessionID]; !custom {
				out.titles[l.SessionID] = l.AITitle
			}
		case "assistant":
			m := l.Message
			if m == nil || m.Usage == nil || m.Model == "" || m.Model == "<synthetic>" || l.Timestamp.IsZero() {
				return
			}
			u := m.Usage
			if u.Input+u.CacheWrite+u.CacheRead+u.Output == 0 {
				return
			}
			r := extRec{hour: l.Timestamp.Unix() / 3600 * 3600, source: "claude_code", session: l.SessionID, cwd: l.Cwd, model: m.Model,
				in: u.Input + u.CacheWrite + u.CacheRead, cached: u.CacheRead, cacheWrite: u.CacheWrite, out: u.Output, reportedUSD: -1}
			if l.CostUSD != nil {
				r.reportedUSD = *l.CostUSD
			}
			// Claude Code writes one line per content block, each repeating
			// the whole message's usage: keep the first (ccusage does too).
			if m.ID != "" || l.RequestID != "" {
				r.dkey = m.ID + ":" + l.RequestID
			}
			out.recs = append(out.recs, r)
		}
	})
	return out
}

func parseCodexRollout(f *os.File) *extFile {
	out := &extFile{}
	var session, cwd, model string
	var yoake bool
	var forkAt time.Time
	lastTotal := -1
	lines(f, func(b []byte) {
		if !bytes.Contains(b, []byte(`"session_meta"`)) && !bytes.Contains(b, []byte(`"turn_context"`)) && !bytes.Contains(b, []byte(`"total_token_usage"`)) {
			return
		}
		var l struct {
			Timestamp time.Time       `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(b, &l) != nil {
			return
		}
		switch l.Type {
		case "session_meta":
			var p struct {
				ID         string `json:"id"`
				Cwd        string `json:"cwd"`
				Originator string `json:"originator"`
				ForkedFrom string `json:"forked_from_id"`
			}
			_ = json.Unmarshal(l.Payload, &p)
			session, cwd, yoake = p.ID, p.Cwd, p.Originator == "yoakeland"
			if p.ForkedFrom != "" {
				forkAt = l.Timestamp
			}
		case "turn_context":
			var p struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(l.Payload, &p) == nil && p.Model != "" {
				model = p.Model
			}
		case "event_msg":
			var p struct {
				Type string `json:"type"`
				Info *struct {
					Total struct {
						Total int `json:"total_tokens"`
					} `json:"total_token_usage"`
					Last struct {
						Input  int `json:"input_tokens"`
						Cached int `json:"cached_input_tokens"`
						Output int `json:"output_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(l.Payload, &p) != nil || p.Type != "token_count" || p.Info == nil {
				return
			}
			// token_count repeats unchanged between requests; a new request
			// moves the running total.
			if p.Info.Total.Total == lastTotal {
				return
			}
			lastTotal = p.Info.Total.Total
			// A fork opens with its parent's history copied in and re-stamped
			// within a second of the fork (T3 Code/ccusage use the same cut).
			// ponytail: timing heuristic; exact if Codex ever marks copies.
			if !forkAt.IsZero() && l.Timestamp.Sub(forkAt) < time.Second {
				return
			}
			u := p.Info.Last
			if u.Input == 0 && u.Output == 0 {
				return
			}
			out.recs = append(out.recs, extRec{hour: l.Timestamp.Unix() / 3600 * 3600, source: "codex", session: session, cwd: cwd, model: model,
				in: u.Input, cached: u.Cached, out: u.Output, reportedUSD: -1, yoake: yoake})
		}
	})
	return out
}

// codexThreadNames reads Codex's own index of thread titles.
func codexThreadNames() map[string]string {
	out := map[string]string{}
	f, err := os.Open(filepath.Join(codexDir(), "session_index.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	lines(f, func(b []byte) {
		var l struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(b, &l) == nil && l.ID != "" && l.Name != "" {
			out[l.ID] = l.Name
		}
	})
	return out
}
