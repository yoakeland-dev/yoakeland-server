package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"yoakeland/internal/artifact"
	"yoakeland/internal/protocol"
)

// An artifact page is model-written code, so what it sends is bounded: a few events a second
// per connection, and one submit (it starts a model turn) per artifact: a page answers once, and
// anything it sends after that is dropped without a word.
const (
	artifactEventsPerSec  = 20
	maxSubmitsPerArtifact = 1
	maxArtifactIDLen      = 120
)

var artifactGate = struct {
	sync.Mutex
	window  map[string]int // peer -> events in the current second
	second  map[string]int64
	submits map[string]int // session+artifact -> submits so far
}{window: map[string]int{}, second: map[string]int64{}, submits: map[string]int{}}

// artifactAllow counts one event from peer and reports whether it is within the rate.
func artifactAllow(peerID string, now time.Time) bool {
	g := &artifactGate
	g.Lock()
	defer g.Unlock()
	if s := now.Unix(); g.second[peerID] != s {
		g.second[peerID], g.window[peerID] = s, 0
	}
	g.window[peerID]++
	return g.window[peerID] <= artifactEventsPerSec
}

// handleArtifactEnvelope answers artifact_event; it reports whether env was one. Allowed for a
// restricted paired device: it is only chat input, like a typed message.
func handleArtifactEnvelope(ipc *protocol.IPCStream, deps connDeps, peerID string, env *protocol.Envelope) bool {
	if env.Type != protocol.EventArtifactEvent {
		return false
	}
	fail := func(msg string) {
		_ = ipc.SendEvent(protocol.EventError, env.SessionID, env.TurnID, "", protocol.ErrorData{Message: msg})
	}
	var d protocol.ArtifactEventData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		fail("malformed artifact_event payload")
		return true
	}
	if d.SessionID == "" {
		d.SessionID = env.SessionID
	}
	if !artifactAllow(peerID, time.Now()) {
		return true // a page spamming events gets silence, not an error per event
	}
	if d.SessionID == "" || d.ArtifactID == "" || len(d.ArtifactID) > maxArtifactIDLen || strings.ContainsAny(d.ArtifactID, "\r\n\x00") {
		fail("artifact_event needs a sessionId and a short artifactId")
		return true
	}
	body, err := json.Marshal(d.Data)
	if err != nil || len(body) > protocol.MaxArtifactData {
		fail(fmt.Sprintf("artifact data is over %d bytes", protocol.MaxArtifactData))
		return true
	}
	switch d.Kind {
	case protocol.ArtifactLog, protocol.ArtifactError:
		text := string(body)
		if s, ok := d.Data.(string); ok {
			text = s
		}
		artifact.Default.Add(d.SessionID, d.ArtifactID, d.Kind, text)
	case protocol.ArtifactSubmit:
		if deps.sched == nil {
			fail("artifact submits aren't available on this server")
			return true
		}
		key := d.SessionID + "\x00" + d.ArtifactID
		artifactGate.Lock()
		artifactGate.submits[key]++
		over := artifactGate.submits[key] > maxSubmitsPerArtifact
		artifactGate.Unlock()
		if over {
			return true
		}
		_, err := deps.sched.Add(protocol.ScheduleTurnData{
			SessionID: d.SessionID, ProjectPath: d.ProjectPath,
			Text:    fmt.Sprintf("[The user acted in the artifact %s you wrote. What the page sent:]\n%s", d.ArtifactID, body),
			Trigger: protocol.ScheduleTrigger{Kind: protocol.TriggerNow},
			Origin:  &protocol.Origin{Kind: protocol.OriginArtifact, ArtifactID: d.ArtifactID},
		})
		if err != nil {
			fail(err.Error())
		}
	default:
		fail(fmt.Sprintf("unknown artifact_event kind %q", d.Kind))
	}
	return true
}
