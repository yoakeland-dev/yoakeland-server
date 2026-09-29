package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildUsageReport(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "projects", "abc")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "sessions"), 0755))
	must(os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"path":"/p"}`), 0644))
	// m2 in history, m3 only on an archived branch, m2 repeated there too,
	// and a subagent round-trip nested in a tool message.
	must(os.WriteFile(filepath.Join(dir, "sessions", "s1.json"), []byte(`{
		"sessionId":"s1","title":"T","history":[
			{"id":"m1","role":"user"},
			{"id":"m2","provider":"A","model":"x","createdAt":"2026-09-20T10:15:00Z","generationMs":100,
			 "usage":{"inputTokens":100,"outputTokens":10,"cachedInputTokens":40,"costUsd":0.5}},
			{"id":"m4","role":"tool","subagent":[
				{"id":"m1","provider":"A","model":"y","createdAt":"2026-09-20T10:40:00Z","usage":{"inputTokens":7,"outputTokens":3,"costUsd":0.1}}]}
		],
		"branches":{"m1":[[
			{"id":"m2","provider":"A","model":"x","createdAt":"2026-09-20T10:15:00Z","usage":{"inputTokens":100,"outputTokens":10,"costUsd":0.5}},
			{"id":"m3","provider":"A","model":"x","createdAt":"2026-09-19T08:00:00Z","usage":{"inputTokens":50,"outputTokens":5,"costUsd":0.25}}
		]]}
	}`), 0644))
	must(os.WriteFile(filepath.Join(dir, "usage.json"), []byte(`{"sessions":{
		"s1":{"inputTokens":1,"outputTokens":1,"costUsd":1},
		"gone":{"inputTokens":9,"outputTokens":2,"costUsd":0.75}}}`), 0644))

	r := buildUsageReport(state, map[string]string{"A": "api_key"}, time.Time{}, false, nil)
	var in, cached, out, n int
	var usd float64
	for _, row := range r.Rows {
		in, cached, out, n, usd = in+row.InputTokens, cached+row.CachedTokens, out+row.OutputTokens, n+row.Requests, usd+row.CostUSD
	}
	if n != 3 || in != 157 || cached != 40 || out != 18 || usd < 0.849 || usd > 0.851 {
		t.Fatalf("totals n=%d in=%d cached=%d out=%d usd=%v", n, in, cached, out, usd)
	}
	if len(r.Sessions) != 1 || r.Sessions[0].ProjectPath != "/p" || r.Sessions[0].Title != "T" {
		t.Fatalf("sessions %+v", r.Sessions)
	}
	if len(r.Providers) != 1 || r.Providers[0].Kind != "api_key" || len(r.Models) != 2 {
		t.Fatalf("providers %+v models %v", r.Providers, r.Models)
	}
	if r.Deleted.Sessions != 1 || r.Deleted.CostUSD != 0.75 {
		t.Fatalf("deleted %+v", r.Deleted)
	}
	if r.FirstAt != "2026-09-19T08:00:00Z" {
		t.Fatalf("firstAt %s", r.FirstAt)
	}
	// Since drops the 09-19 branch round-trip; firstAt still reports it.
	r = buildUsageReport(state, nil, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), false, nil)
	n = 0
	for _, row := range r.Rows {
		n += row.Requests
		if row.Hour != time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC).Unix() {
			t.Fatalf("hour %d", row.Hour)
		}
	}
	if n != 2 || r.FirstAt != "2026-09-19T08:00:00Z" {
		t.Fatalf("since: n=%d firstAt=%s", n, r.FirstAt)
	}
}
