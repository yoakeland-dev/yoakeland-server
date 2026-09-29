package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Two content blocks of one message (same id:requestId) count once; a
	// session yoakeland drove ("yk", its native id) is skipped.
	write(filepath.Join(home, "claude", "projects", "p", "a.jsonl"), `{"type":"custom-title","customTitle":"Mine","sessionId":"cc1"}
{"type":"assistant","timestamp":"2026-09-20T10:05:00Z","sessionId":"cc1","cwd":"/w","requestId":"r1","message":{"id":"m1","model":"claude-x","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":70,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-09-20T10:05:01Z","sessionId":"cc1","cwd":"/w","requestId":"r1","message":{"id":"m1","model":"claude-x","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":70,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-09-20T10:06:00Z","sessionId":"yk","cwd":"/w","requestId":"r2","message":{"id":"m2","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}
{"type":"user","message":{"content":"x"}}
`)
	// A repeated token_count (unchanged total) counts once; a yoakeland
	// rollout is skipped.
	write(filepath.Join(home, "codex", "sessions", "2026", "a.jsonl"), `{"timestamp":"2026-09-20T11:00:00Z","type":"session_meta","payload":{"id":"cx1","cwd":"/c","originator":"codex-tui"}}
{"timestamp":"2026-09-20T11:00:01Z","type":"turn_context","payload":{"model":"gpt-x"}}
{"timestamp":"2026-09-20T11:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":10}}}}
{"timestamp":"2026-09-20T11:00:06Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":10}}}}
`)
	write(filepath.Join(home, "codex", "sessions", "2026", "b.jsonl"), `{"timestamp":"2026-09-20T11:00:00Z","type":"session_meta","payload":{"id":"cx2","cwd":"/c","originator":"yoakeland"}}
{"timestamp":"2026-09-20T11:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":5},"last_token_usage":{"input_tokens":4,"output_tokens":1}}}}
`)
	write(filepath.Join(home, "codex", "session_index.jsonl"), `{"id":"cx1","thread_name":"Codex thread"}`+"\n")
	state := filepath.Join(home, "state")
	write(filepath.Join(state, "projects", "x", "sessions", "s.json"), `{"sessionId":"s","native":{"provider":"claude","sessionId":"yk"},"history":[]}`)

	price := func(model string, in, cr, cw, out int) float64 { return float64(in-cr-cw)*1 + float64(cr)*0.1 + float64(cw)*1.25 + float64(out)*5 }
	r := buildUsageReport(state, nil, time.Time{}, true, price)
	got := map[string][4]float64{}
	for _, row := range r.Rows {
		src := r.Sessions[row.Session].Source
		g := got[src]
		got[src] = [4]float64{g[0] + float64(row.Requests), g[1] + float64(row.InputTokens), g[2] + float64(row.CachedTokens), g[3] + row.CostUSD}
	}
	if g := got["claude_code"]; g != [4]float64{1, 100, 70, 10 + 7 + 25 + 25} {
		t.Fatalf("claude %v", g)
	}
	if g := got["codex"]; g != [4]float64{1, 100, 60, 40 + 6 + 50} {
		t.Fatalf("codex %v", g)
	}
	titles := map[string]string{}
	for _, s := range r.Sessions {
		titles[s.ID] = s.Title
	}
	if titles["cc1"] != "Mine" || titles["cx1"] != "Codex thread" {
		t.Fatalf("titles %v", titles)
	}
	// Without external, none of it shows.
	if r := buildUsageReport(state, nil, time.Time{}, false, price); len(r.Rows) != 0 {
		t.Fatalf("external off: %d rows", len(r.Rows))
	}
}
