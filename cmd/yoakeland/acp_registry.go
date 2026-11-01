package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"yoakeland/internal/protocol"
)

// The registry sends no CORS headers, so a web client asks the server. The URL is fixed: a peer
// can't point the server at anything else.
var acpRegistryURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

var acpCache struct {
	mu     sync.Mutex
	at     time.Time
	agents []protocol.ACPAgent
	failAt time.Time // a failed fetch is remembered for 30s, so a down registry can't queue up fetches
	fail   error
}

// acpAgents returns the registry's agents the app can launch (npx, uvx), cached for ten minutes
// (a failure for thirty seconds).
func acpAgents(ctx context.Context) ([]protocol.ACPAgent, error) {
	acpCache.mu.Lock()
	defer acpCache.mu.Unlock()
	if time.Since(acpCache.at) < 10*time.Minute {
		return acpCache.agents, nil
	}
	if acpCache.fail != nil && time.Since(acpCache.failAt) < 30*time.Second {
		return nil, acpCache.fail
	}
	agents, err := fetchACPAgents(ctx)
	if err != nil {
		acpCache.failAt, acpCache.fail = time.Now(), err
		return nil, err
	}
	acpCache.at, acpCache.agents, acpCache.fail = time.Now(), agents, nil
	return agents, nil
}

func fetchACPAgents(ctx context.Context) ([]protocol.ACPAgent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, acpRegistryURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the registry answered %d", res.StatusCode)
	}
	var doc struct {
		Agents []struct {
			protocol.ACPAgent
			Distribution struct {
				Npx json.RawMessage `json:"npx"`
				Uvx json.RawMessage `json:"uvx"`
			} `json:"distribution"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("the registry sent something unreadable: %w", err)
	}
	agents := make([]protocol.ACPAgent, 0, len(doc.Agents))
	for _, a := range doc.Agents {
		a.ACPAgent.Npx, a.ACPAgent.Uvx = a.Distribution.Npx, a.Distribution.Uvx
		agents = append(agents, a.ACPAgent)
	}
	return agents, nil
}

// handleListACPRegistry answers list_acp_registry without holding up the connection's other requests.
func handleListACPRegistry(ctx context.Context, ipc *protocol.IPCStream, env *protocol.Envelope) {
	var req struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(env.Data, &req)
	go func() {
		out := protocol.ACPRegistryData{RequestID: req.RequestID}
		agents, err := acpAgents(ctx)
		if err != nil {
			out.Error = err.Error()
		}
		out.Agents = agents
		_ = ipc.SendEvent(protocol.EventACPRegistry, env.SessionID, env.TurnID, "", out)
	}()
}
