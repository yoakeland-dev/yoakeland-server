package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"yoakeland/internal/protocol"
)

// maxAvatarLen caps AuthData.Avatar/SetPeerAvatarData.Avatar (a data URL) at
// a generous multiple of what a 96x96 thumbnail actually needs (typically a
// few KB), since this value gets stored and rebroadcast to every peer on
// every roster change — a defensive bound against one bad client bloating
// everyone else's peer_list, not a real image-processing limit.
const maxAvatarLen = 200_000

// Peer is one live connection to this server process — see PeerInfo for the
// wire shape. ID is stable per-connection, not per-user; TokenName/Email
// are the resolved identity of a per-person token (see register_token),
// empty for unix/unauthenticated-by-token connections.
type Peer struct {
	ID          string
	IP          string
	ConnectedAt time.Time
	Name        string
	DeviceType  string
	Avatar      string
	TokenName   string
	Email       string
	DeviceID    string
	SessionID   string // last chat opened, see SetActivity
	ProjectPath string
	Network     string // "unix" or "tcp"
	// BrowserHost: offered its browser panel to the agent (browser_host).
	BrowserHost bool

	// Presence is the last set_presence heartbeat (see PeerRegistry.Viewing).
	// Not in PeerInfo: it changes too often to broadcast.
	presence presenceState

	conn io.Closer
	ipc  *protocol.IPCStream
}

// PeerRegistry tracks every connection currently attached to this server
// (unix and TCP alike; -stdio's single pipe never registers one — see
// acceptLoop) and pushes a fresh peer_list to everyone whenever the roster
// changes, per the server checklist's "push-on-change is better than a poll"
// call.
type PeerRegistry struct {
	mu    sync.Mutex
	peers map[string]*Peer
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{peers: make(map[string]*Peer)}
}

// peerIdentity is who a connection says it is (its auth frame), resolved
// against its token on TCP. See handshake.
type peerIdentity struct {
	Name, DeviceType, Avatar, DeviceID string
	TokenName, Email                   string // set only for a named TCP token
}

// maxNameLen caps a display name; cleanName trims it and drops control characters, since a name
// is shown in every peer's roster and chat.
const maxNameLen = 64

func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxNameLen {
		s = string(r[:maxNameLen])
	}
	return s
}

// Add registers a freshly handshaken connection and broadcasts the updated
// roster. An empty name gets a generated placeholder so the peer list never
// shows a blank row; an oversized avatar — see maxAvatarLen — is dropped
// rather than failing the connection over what's cosmetic.
func (r *PeerRegistry) Add(ipc *protocol.IPCStream, conn io.Closer, network string, who peerIdentity) *Peer {
	id := randomID()
	who.Name = cleanName(who.Name)
	if who.Name == "" {
		who.Name = who.DeviceType + "-" + id[:6]
	}
	if len(who.Avatar) > maxAvatarLen {
		who.Avatar = ""
	}
	name, deviceType := who.Name, who.DeviceType

	p := &Peer{
		ID:          id,
		IP:          remoteIP(conn, network),
		ConnectedAt: time.Now().UTC(),
		Name:        who.Name,
		DeviceType:  who.DeviceType,
		Avatar:      who.Avatar,
		DeviceID:    who.DeviceID,
		TokenName:   who.TokenName,
		Email:       who.Email,
		Network:     network,
		conn:        conn,
		ipc:         ipc,
	}

	r.mu.Lock()
	r.peers[id] = p
	r.mu.Unlock()
	logInfo("peer connected: %s %q (%s) from %s over %s", id, name, deviceType, p.IP, network)
	r.Broadcast()
	return p
}

// Remove unregisters a connection (on disconnect, however it happened) and
// broadcasts the updated roster.
func (r *PeerRegistry) Remove(id string) {
	r.mu.Lock()
	_, existed := r.peers[id]
	delete(r.peers, id)
	r.mu.Unlock()
	if existed {
		logInfo("peer disconnected: %s", id)
		r.Broadcast()
	}
}

// Rename changes id's own display name (a peer may only rename itself — see
// set_peer_name) and broadcasts the updated roster. Reports whether id was a
// live peer.
func (r *PeerRegistry) Rename(id, name string) bool {
	name = cleanName(name)
	r.mu.Lock()
	p, ok := r.peers[id]
	if ok && name != "" {
		p.Name = name
	}
	r.mu.Unlock()
	if ok {
		r.Broadcast()
	}
	return ok
}

// SetAvatar changes id's own avatar (a peer may only set its own — see
// set_peer_avatar) and broadcasts the updated roster. Reports whether id was
// a live peer; the caller is responsible for rejecting an oversized avatar
// with a proper error before calling this (see maxAvatarLen) — this method
// stays as unopinionated as Rename.
func (r *PeerRegistry) SetAvatar(id, avatar string) bool {
	r.mu.Lock()
	p, ok := r.peers[id]
	if ok {
		p.Avatar = avatar
	}
	r.mu.Unlock()
	if ok {
		r.Broadcast()
	}
	return ok
}

// Disconnect force-closes id's connection. The owning acceptLoop goroutine's
// deferred Remove picks up the resulting close and broadcasts the roster
// update itself — no need to do that here too. Reports whether id was a live
// peer.
func (r *PeerRegistry) Disconnect(id string) bool {
	r.mu.Lock()
	p, ok := r.peers[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	_ = p.conn.Close()
	return true
}

// DisconnectByToken force-closes every live connection currently
// authenticated under tokenName — remove_token's other half: deleting the
// credential only stops *future* handshakes, so a revoked person would
// otherwise stay connected on the session they already have (same reasoning
// as remoteControl.Stop force-closing everyone when remote access itself
// turns off). Reports how many connections that was.
func (r *PeerRegistry) DisconnectByToken(tokenName string) int {
	r.mu.Lock()
	var toClose []io.Closer
	for _, p := range r.peers {
		if p.TokenName == tokenName {
			toClose = append(toClose, p.conn)
		}
	}
	r.mu.Unlock()
	for _, c := range toClose {
		_ = c.Close()
	}
	return len(toClose)
}

// Author is a peer's identity as attribution needs it, read under the lock because Name and
// Avatar change live.
func (r *PeerRegistry) Author(id string) (name, avatar, credential, email string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, found := r.peers[id]
	if !found {
		return "", "", "", "", false
	}
	return p.Name, p.Avatar, p.TokenName, p.Email, true
}

// Get looks up a live peer by ID, for callers that need one specific peer rather than the whole
// roster. Name, Avatar and SessionID change live: read them through Author or Identify.
func (r *PeerRegistry) Get(id string) (*Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	return p, ok
}

// SetBrowserHost records whether id offers its browser panel to the agent.
func (r *PeerRegistry) SetBrowserHost(id string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.peers[id]; ok {
		p.BrowserHost = enabled
	}
}

// SetActivity records the chat id last opened, broadcasting only on a
// change. Reports whether id was a live peer.
func (r *PeerRegistry) SetActivity(id, sessionID, projectPath string) bool {
	r.mu.Lock()
	p, ok := r.peers[id]
	changed := ok && (p.SessionID != sessionID || p.ProjectPath != projectPath)
	if changed {
		p.SessionID, p.ProjectPath = sessionID, projectPath
	}
	r.mu.Unlock()
	if changed {
		r.Broadcast()
	}
	return ok
}

// presenceTTL is how long a set_presence heartbeat counts. Clients repeat it
// every minute, so a longer silence means the device is asleep or gone.
const presenceTTL = 5 * time.Minute

type presenceState struct {
	sessions         []string
	focused, visible bool
	at               time.Time
}

// SetPresence stores a device's heartbeat. Reports whether id was a live peer.
func (r *PeerRegistry) SetPresence(id string, sessions []string, focused, visible bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if ok {
		p.presence = presenceState{slices.Clone(sessions), focused, visible, time.Now()}
	}
	return ok
}

// Viewing reports whether some device shows sessionID in a focused, visible
// window right now.
func (r *PeerRegistry) Viewing(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	return r.anyPresent(func(s presenceState) bool { return slices.Contains(s.sessions, sessionID) })
}

// Attended reports whether any device is in use right now.
func (r *PeerRegistry) Attended() bool {
	return r.anyPresent(func(presenceState) bool { return true })
}

func (r *PeerRegistry) anyPresent(match func(presenceState) bool) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.peers {
		s := p.presence
		if s.focused && s.visible && time.Since(s.at) < presenceTTL && match(s) {
			return true
		}
	}
	return false
}

// All returns every live peer, for callers (like a server_identity push)
// that need to reach every connection rather than just the roster shape.
func (r *PeerRegistry) All() []*Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	return out
}

// Count is how many peers are connected right now.
func (r *PeerRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

// BroadcastEvent pushes one event to every live peer except exceptPeerID —
// the requester, who already got its own direct response inline. This is
// the live-sync half of chat-list/project-list consistency: any other
// connection currently attached to this process learns about a mutation
// (a session's title/archive/delete, a project opened/removed) made
// elsewhere without polling. exceptPeerID may be "" to exclude nobody.
func (r *PeerRegistry) BroadcastEvent(exceptPeerID string, eventType protocol.EventType, sessionID string, data any) {
	r.mu.Lock()
	peers := make([]*Peer, 0, len(r.peers))
	for id, p := range r.peers {
		if id == exceptPeerID {
			continue
		}
		peers = append(peers, p)
	}
	r.mu.Unlock()

	for _, p := range peers {
		_ = p.ipc.SendEvent(eventType, sessionID, "", "", data)
	}
}

// HasLocal reports whether any client is connected over the local socket (one that could approve
// a pairing).
func (r *PeerRegistry) HasLocal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.peers {
		if p.Network == "unix" {
			return true
		}
	}
	return false
}

// BroadcastLocal sends an event to the local-socket peers only.
func (r *PeerRegistry) BroadcastLocal(eventType protocol.EventType, data any) {
	r.mu.Lock()
	var peers []*Peer
	for _, p := range r.peers {
		if p.Network == "unix" {
			peers = append(peers, p)
		}
	}
	r.mu.Unlock()
	for _, p := range peers {
		_ = p.ipc.SendEvent(eventType, "", "", "", data)
	}
}

// Broadcast pushes the current roster to every live peer.
func (r *PeerRegistry) Broadcast() {
	r.mu.Lock()
	list := make([]protocol.PeerInfo, 0, len(r.peers))
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		list = append(list, protocol.PeerInfo{
			PeerID:      p.ID,
			Name:        p.Name,
			Credential:  p.TokenName,
			IP:          p.IP,
			DeviceType:  p.DeviceType,
			Avatar:      p.Avatar,
			ConnectedAt: p.ConnectedAt,
			DeviceID:    p.DeviceID,
			SessionID:   p.SessionID,
			ProjectPath: p.ProjectPath,
		})
		peers = append(peers, p)
	}
	r.mu.Unlock()

	for _, p := range peers {
		_ = p.ipc.SendEvent(protocol.EventPeerList, "", "", "", protocol.PeerListData{Peers: list, You: p.ID})
	}
}

// remoteIP extracts a bare IP (no port) from a network connection's remote
// address. Unix connections have no meaningful remote IP.
func remoteIP(conn io.Closer, network string) string {
	if network == "unix" {
		return ""
	}
	nc, ok := conn.(net.Conn)
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(nc.RemoteAddr().String())
	if err != nil {
		return nc.RemoteAddr().String()
	}
	return host
}

// randomID generates a short random hex identifier for a peer. Falls back
// to a timestamp on the astronomically unlikely chance crypto/rand fails —
// peer IDs are a display/lookup key, not a security credential, so this
// doesn't need the hard-fail treatment setupTCPListener gives the auth token.
func randomID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf)
}
