package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"yoakeland/internal/protocol"
)

// get_usage (SPEC.md §3.1) reads usage straight from the session files:
// every assistant message already carries its round-trip's provider, model,
// time and usage, archived branches and subagent transcripts included, so
// history from before this request existed is covered too.

type usageMsg struct {
	ID           string              `json:"id"`
	Provider     string              `json:"provider"`
	Model        string              `json:"model"`
	CreatedAt    time.Time           `json:"createdAt"`
	GenerationMs int64               `json:"generationMs"`
	Usage        *protocol.UsageData `json:"usage"`
	Subagent     []usageMsg          `json:"subagent"`
}

type usageSessionFile struct {
	SessionID string                  `json:"sessionId"`
	CreatedAt time.Time               `json:"createdAt"`
	Title     string                  `json:"title"`
	Archived  bool                    `json:"archived"`
	History   []usageMsg              `json:"history"`
	Branches  map[string][][]usageMsg `json:"branches"`
	Native    *struct {
		SessionID string `json:"sessionId"`
	} `json:"native"`
}

type usageRec struct {
	hour            int64
	provider, model string
	u               protocol.UsageData
	ms              int64
}

type usageParsed struct {
	mod    time.Time
	size   int64
	sess   protocol.UsageSession
	recs   []usageRec
	native string // the CLI's own session id, for a chat run on a CLI agent
}

// usageFiles caches each session file's records by mtime and size, so a
// repeat get_usage only re-parses sessions that changed.
var usageFiles = struct {
	sync.Mutex
	m map[string]*usageParsed
}{m: map[string]*usageParsed{}}

func parseUsageSession(path string, fi os.FileInfo, projectPath string) *usageParsed {
	usageFiles.Lock()
	c := usageFiles.m[path]
	usageFiles.Unlock()
	if c != nil && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f usageSessionFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	if f.SessionID == "" {
		f.SessionID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	p := &usageParsed{mod: fi.ModTime(), size: fi.Size(),
		sess: protocol.UsageSession{ID: f.SessionID, Title: f.Title, ProjectPath: projectPath, Archived: f.Archived}}
	var walk func(msgs []usageMsg, seen map[string]bool)
	walk = func(msgs []usageMsg, seen map[string]bool) {
		for _, m := range msgs {
			// An id seen twice is one message, not two round-trips.
			if seen != nil && m.ID != "" {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
			}
			if m.Usage != nil && (m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0 || m.Usage.CostUSD > 0) {
				at := m.CreatedAt
				if at.IsZero() {
					at = f.CreatedAt
				}
				p.recs = append(p.recs, usageRec{hour: at.Unix() / 3600 * 3600, provider: m.Provider, model: m.Model, u: *m.Usage, ms: m.GenerationMs})
			}
			walk(m.Subagent, nil)
		}
	}
	seen := map[string]bool{}
	walk(f.History, seen)
	for _, alts := range f.Branches {
		for _, br := range alts {
			walk(br, seen)
		}
	}
	if f.Native != nil {
		p.native = f.Native.SessionID
	}
	usageFiles.Lock()
	usageFiles.m[path] = p
	usageFiles.Unlock()
	return p
}

// usagePricer prices a request with cache reads/writes split out of input.
type usagePricer func(model string, input, cacheRead, cacheWrite, output int) float64

// buildUsageReport scans stateDir itself (pre-project-layout servers kept
// sessions there) and every projects/<id> data dir, known or since removed,
// then with external the CLIs' own transcripts. providerKinds maps
// registered provider names to their kind.
func buildUsageReport(stateDir string, providerKinds map[string]string, since time.Time, external bool, price usagePricer) protocol.UsageReportData {
	out := protocol.UsageReportData{Sessions: []protocol.UsageSession{}, Providers: []protocol.UsageProvider{}, Models: []string{}, Rows: []protocol.UsageRow{}, Accounts: []protocol.UsageAccount{}}
	dirs, _ := filepath.Glob(filepath.Join(stateDir, "projects", "*"))
	dirs = append([]string{stateDir}, dirs...)

	sessIdx, provIdx, modelIdx := map[string]int{}, map[string]int{}, map[string]int{}
	type rowKey struct {
		hour    int64
		s, p, m int
	}
	rows := map[rowKey]*protocol.UsageRow{}
	var first int64
	add := func(hour int64, sess protocol.UsageSession, prov, kind, model string, n int, u protocol.UsageData, ms int64) {
		if first == 0 || hour < first {
			first = hour
		}
		if !since.IsZero() && hour+3600 <= since.Unix() {
			return
		}
		sk := sess.Source + "\x00" + sess.ProjectPath + "\x00" + sess.ID
		si, ok := sessIdx[sk]
		if !ok {
			si = len(out.Sessions)
			sessIdx[sk] = si
			out.Sessions = append(out.Sessions, sess)
		}
		pi, ok := provIdx[prov]
		if !ok {
			pi = len(out.Providers)
			provIdx[prov] = pi
			out.Providers = append(out.Providers, protocol.UsageProvider{Name: prov, Kind: kind})
		}
		mi, ok := modelIdx[model]
		if !ok {
			mi = len(out.Models)
			modelIdx[model] = mi
			out.Models = append(out.Models, model)
		}
		k := rowKey{hour, si, pi, mi}
		row := rows[k]
		if row == nil {
			row = &protocol.UsageRow{Hour: hour, Session: si, Provider: pi, Model: mi}
			rows[k] = row
		}
		row.Requests += n
		row.InputTokens += u.InputTokens
		row.CachedTokens += u.CachedInputTokens
		row.OutputTokens += u.OutputTokens
		row.CostUSD += u.CostUSD
		row.GenerationMs += ms
	}

	native := map[string]bool{}
	for _, dir := range dirs {
		projectPath := ""
		if b, err := os.ReadFile(filepath.Join(dir, "project.json")); err == nil {
			var pj struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(b, &pj)
			projectPath = pj.Path
		}
		present := map[string]bool{}
		files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
		for _, path := range files {
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			p := parseUsageSession(path, fi, projectPath)
			if p == nil {
				continue
			}
			present[p.sess.ID] = true
			if p.native != "" {
				native[p.native] = true
			}
			for _, r := range p.recs {
				add(r.hour, p.sess, r.provider, providerKinds[r.provider], r.model, 1, r.u, r.ms)
			}
		}
		// usage.json outlives a deleted session's file.
		if b, err := os.ReadFile(filepath.Join(dir, "usage.json")); err == nil {
			var uj struct {
				Sessions map[string]protocol.UsageData `json:"sessions"`
			}
			_ = json.Unmarshal(b, &uj)
			for id, t := range uj.Sessions {
				if present[id] || (t.InputTokens == 0 && t.OutputTokens == 0 && t.CostUSD == 0) {
					continue
				}
				out.Deleted.Sessions++
				out.Deleted.InputTokens += t.InputTokens
				out.Deleted.OutputTokens += t.OutputTokens
				out.Deleted.CostUSD += t.CostUSD
			}
		}
	}

	if external {
		recs, titles := scanExternal()
		seen := map[string]bool{}
		for _, r := range recs {
			// A chat yoakeland ran on a CLI is already counted above.
			if r.yoake || native[r.session] {
				continue
			}
			if r.dkey != "" {
				if seen[r.dkey] {
					continue
				}
				seen[r.dkey] = true
			}
			usd := r.reportedUSD
			if usd < 0 {
				usd = 0
				if price != nil {
					usd = price(r.model, r.in, r.cached, r.cacheWrite, r.out)
				}
			}
			kind := "cli_claude"
			if r.source == "codex" {
				kind = "cli_codex"
			}
			sess := protocol.UsageSession{ID: r.session, Title: titles[r.session], ProjectPath: r.cwd, Source: r.source}
			add(r.hour, sess, r.source, kind, r.model, 1, protocol.UsageData{InputTokens: r.in, CachedInputTokens: r.cached, OutputTokens: r.out, CostUSD: usd}, 0)
		}
	}

	for _, r := range rows {
		out.Rows = append(out.Rows, *r)
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].Hour < out.Rows[j].Hour })
	if first > 0 {
		out.FirstAt = time.Unix(first, 0).UTC().Format(time.RFC3339)
	}
	return out
}
