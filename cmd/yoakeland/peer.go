package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"time"
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

// Add registers a freshly handshaken connection and broadcasts the updated
// roster. name/deviceType/avatar come from the connection's auth frame
// (empty name gets a generated placeholder so the peer list never shows a
// blank row; an oversized avatar — see maxAvatarLen — is dropped rather than
// failing the connection over what's cosmetic).
func (r *PeerRegistry) Add(ipc *protocol.IPCStream, conn io.Closer, network, name, deviceType, avatar string) *Peer {
	id := randomID()
	if name == "" {
		name = deviceType + "-" + id[:6]
	}
	if len(avatar) > maxAvatarLen {
		avatar = ""
	}

	p := &Peer{
		ID:          id,
		IP:          remoteIP(conn, network),
		ConnectedAt: time.Now().UTC(),
		Name:        name,
		DeviceType:  deviceType,
		Avatar:      avatar,
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
	r.mu.Lock()
	p, ok := r.peers[id]
	if ok {
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

// Get looks up a live peer by ID, for callers (like the user_prompt author
// lookup, or set_peer_name's tokened-peer guard) that need one specific
// peer's current identity rather than the whole roster.
func (r *PeerRegistry) Get(id string) (*Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	return p, ok
}

// SetTokenIdentity records which named token (see register_token)
// authenticated id, plus its cached email — purely internal bookkeeping for
// message/git-author attribution, not part of the peer_list wire shape, so
// this deliberately doesn't broadcast. Reports whether id was a live peer.
func (r *PeerRegistry) SetTokenIdentity(id, tokenName, email string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if ok {
		p.TokenName = tokenName
		p.Email = email
	}
	return ok
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

// Broadcast pushes the current roster to every live peer.
func (r *PeerRegistry) Broadcast() {
	r.mu.Lock()
	list := make([]protocol.PeerInfo, 0, len(r.peers))
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		list = append(list, protocol.PeerInfo{
			PeerID:      p.ID,
			Name:        p.Name,
			IP:          p.IP,
			DeviceType:  p.DeviceType,
			Avatar:      p.Avatar,
			ConnectedAt: p.ConnectedAt,
		})
		peers = append(peers, p)
	}
	r.mu.Unlock()

	for _, p := range peers {
		_ = p.ipc.SendEvent(protocol.EventPeerList, "", "", "", protocol.PeerListData{Peers: list})
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
