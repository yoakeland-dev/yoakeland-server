package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"yoakeland/internal/protocol"
)

// ErrUsageUnsupported is returned by ProbeRateLimits for a CLI that has no
// way to report its subscription usage (every ACP agent today).
var ErrUsageUnsupported = errors.New("this CLI doesn't report subscription usage")

// ProbeRateLimits asks kind's CLI for the account's current subscription
// usage without starting a turn: no prompt is sent and no model usage is
// spent. Claude answers Claude Code's `get_usage` control request (the Agent
// SDK's experimental usage() call, the same data `/usage` shows); Codex its
// app-server's `account/rateLimits/read`. Each probe is one short-lived
// process, killed when ctx ends.
func ProbeRateLimits(ctx context.Context, kind CLIKind, configDir string) (*protocol.ProviderRateLimits, error) {
	return probeRateLimits(ctx, kind, envStarter(startProc, procEnv(kind, configDir, nil)))
}

func probeRateLimits(ctx context.Context, kind CLIKind, start procStarter) (*protocol.ProviderRateLimits, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Home, not a project: nothing here reads the working tree.
	dir, _ := os.UserHomeDir()
	switch kind {
	case CLIKindClaude:
		return probeClaudeUsage(ctx, start, dir)
	case CLIKindCodex:
		return probeCodexUsage(ctx, start, dir)
	}
	return nil, ErrUsageUnsupported
}

func probeClaudeUsage(ctx context.Context, start procStarter, dir string) (*protocol.ProviderRateLimits, error) {
	// No MCP servers, settings hooks or transcript: the process only has to
	// live long enough to answer one control request. (--bare would also
	// skip the keychain/OAuth login, which is the account being asked about.)
	proc, err := start(ctx, "claude", []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--strict-mcp-config", "--setting-sources", "",
	}, dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = proc.stdin.Close()
		// Killed by ctx; reap it so it doesn't linger as a zombie.
		if proc.wait != nil {
			go func() { _ = proc.wait() }()
		}
	}()
	// Written while stdout is being read: initialize's answer (every slash
	// command) can outgrow a pipe buffer before the CLI reads the next line.
	go func() {
		var wmu sync.Mutex
		for _, req := range []map[string]any{
			{"type": "control_request", "request_id": "init", "request": map[string]any{"subtype": "initialize"}},
			{"type": "control_request", "request_id": "usage", "request": map[string]any{"subtype": "get_usage", "skip_behaviors": true}},
		} {
			if writeJSONLine(proc.stdin, &wmu, req) != nil {
				return
			}
		}
	}()
	var email string // from initialize's answer, which precedes get_usage's
	for proc.lines.Scan() {
		var ln struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Raw       json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(proc.lines.Bytes(), &ln) != nil || ln.Type != "control_response" {
			continue
		}
		switch ln.Response.RequestID {
		case "init":
			var init struct {
				Account struct {
					Email string `json:"email"`
				} `json:"account"`
			}
			if json.Unmarshal(ln.Response.Raw, &init) == nil {
				email = init.Account.Email
			}
		case "usage":
			if ln.Response.Subtype != "success" {
				return nil, fmt.Errorf("claude get_usage: %s", ln.Response.Error)
			}
			rl, err := parseClaudeUsage(ln.Response.Raw)
			if rl != nil {
				rl.Email = email
			}
			return rl, err
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	msg := "claude exited before answering get_usage"
	if tail := proc.stderr(); tail != "" {
		msg += ": " + lastLines(tail, 5)
	}
	return nil, errors.New(msg)
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (w *claudeUsageWindow) toWindow() *protocol.RateLimitWindow {
	if w == nil || w.Utilization == nil {
		return nil
	}
	win := &protocol.RateLimitWindow{UsedPct: roundPct(*w.Utilization)}
	if w.ResetsAt != nil {
		win.ResetsAt = *w.ResetsAt
	}
	return win
}

// parseClaudeUsage reads a get_usage response. Its percentages are already
// 0–100 (unlike the 0–1 utilization on a streamed rate_limit_event).
// rate_limits also carries a changing set of internal codenamed buckets;
// only the documented ones are read.
func parseClaudeUsage(raw json.RawMessage) (*protocol.ProviderRateLimits, error) {
	var r struct {
		SubscriptionType    *string `json:"subscription_type"`
		RateLimitsAvailable bool    `json:"rate_limits_available"`
		RateLimits          *struct {
			FiveHour       *claudeUsageWindow `json:"five_hour"`
			SevenDay       *claudeUsageWindow `json:"seven_day"`
			SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
			SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
			ModelScoped    []struct {
				DisplayName string `json:"display_name"`
				claudeUsageWindow
			} `json:"model_scoped"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing claude usage: %w", err)
	}
	rl := &protocol.ProviderRateLimits{}
	if r.SubscriptionType != nil {
		rl.Plan = *r.SubscriptionType
	}
	if !r.RateLimitsAvailable || r.RateLimits == nil {
		// API key / Bedrock / Vertex: no plan windows to show.
		return rl, nil
	}
	l := r.RateLimits
	rl.FiveHour = l.FiveHour.toWindow()
	rl.SevenDay = l.SevenDay.toWindow()
	scoped := func(label string, w *claudeUsageWindow) {
		if win := w.toWindow(); win != nil {
			rl.Other = append(rl.Other, protocol.OtherRateLimitWindow{WindowMins: 7 * 24 * 60, Label: label, RateLimitWindow: *win})
		}
	}
	scoped("Opus", l.SevenDayOpus)
	scoped("Sonnet", l.SevenDaySonnet)
	for _, m := range l.ModelScoped {
		scoped(m.DisplayName, &m.claudeUsageWindow)
	}
	return rl, nil
}

func probeCodexUsage(ctx context.Context, start procStarter, dir string) (*protocol.ProviderRateLimits, error) {
	proc, err := start(ctx, "codex", []string{"app-server"}, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = proc.stdin.Close() }()
	var conn *rpcConn
	conn = newRPCConn(proc, func(string, json.RawMessage) {}, func(id json.RawMessage, method string, _ json.RawMessage) {
		conn.respondError(id, -32601, "yoakeland doesn't handle "+method)
	})
	if _, err := conn.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "yoakeland", "version": "1"}, "capabilities": nil}); err != nil {
		return nil, err
	}
	conn.notify("initialized", nil)
	raw, err := conn.call(ctx, "account/rateLimits/read", nil)
	if err != nil {
		return nil, err
	}
	rl, err := parseCodexUsage(raw)
	if rl != nil {
		// Best effort: the email only dedupes, so a failed read isn't a failed probe.
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		defer acancel()
		if acct, aerr := conn.call(actx, "account/read", map[string]any{"refreshToken": false}); aerr == nil {
			var a struct {
				Account struct {
					Email string `json:"email"`
				} `json:"account"`
			}
			if json.Unmarshal(acct, &a) == nil {
				rl.Email = a.Account.Email
			}
		}
	}
	return rl, err
}

// parseCodexUsage reads account/rateLimits/read: the plan's own snapshot,
// plus any extra per-limit buckets (e.g. one scoped to a single model),
// which are labelled by their limitName.
func parseCodexUsage(raw json.RawMessage) (*protocol.ProviderRateLimits, error) {
	var r struct {
		RateLimits          json.RawMessage            `json:"rateLimits"`
		RateLimitsByLimitID map[string]json.RawMessage `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing codex usage: %w", err)
	}
	rl, err := parseCodexRateLimits(r.RateLimits)
	if err != nil {
		return nil, err
	}
	var main struct {
		LimitID string `json:"limitId"`
	}
	_ = json.Unmarshal(r.RateLimits, &main)
	for id, snap := range r.RateLimitsByLimitID {
		if id == main.LimitID {
			continue
		}
		extra, err := parseCodexRateLimits(snap)
		if err != nil {
			continue
		}
		var name struct {
			LimitName *string `json:"limitName"`
		}
		_ = json.Unmarshal(snap, &name)
		label := id
		if name.LimitName != nil && *name.LimitName != "" {
			label = *name.LimitName
		}
		for _, w := range extra.Windows() {
			w.Label = label
			rl.Other = append(rl.Other, w)
		}
	}
	return rl, nil
}
