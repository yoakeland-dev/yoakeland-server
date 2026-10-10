package main

import (
	"errors"
	"sync"
	"time"

	"yoakeland/internal/protocol"
)

// pairingApprovalWait is how long a new device waits for someone at the desktop to say yes.
var pairingApprovalWait = 60 * time.Second

// pairingGate holds the devices waiting to be approved, keyed by the pairing token's name.
type pairingGate struct {
	mu      sync.Mutex
	waiting map[string]chan bool
}

func newPairingGate() *pairingGate { return &pairingGate{waiting: map[string]chan bool{}} }

// answer delivers a local client's decision; false if nothing was waiting under that name.
func (g *pairingGate) answer(credential string, allow bool) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	ch, ok := g.waiting[credential]
	g.mu.Unlock()
	if ok {
		select {
		case ch <- allow:
		default:
		}
	}
	return ok
}

// answerable reports whether a device is currently waiting under that credential.
func (g *pairingGate) answerable(credential string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.waiting[credential]
	return ok
}

// admitPairing decides whether a connection that signed in with a pairing token may continue. A
// token that expired, or that another device already holds, is refused; a device using it for the
// first time waits for a local client to approve (when one is connected to ask), and is then
// bound to the token for good.
func (d connDeps) admitPairing(ipc *protocol.IPCStream, who peerIdentity, rec TokenRecord, ip string) error {
	if !rec.Pairing {
		return nil
	}
	if rec.DeviceID == "" { // first use
		if time.Now().Unix() > rec.ExpiresAt {
			_, _ = d.settings.RemoveToken(who.TokenName)
			return errors.New("this pairing code expired")
		}
		if who.DeviceID == "" {
			return errors.New("this app is too old to pair; update it")
		}
		if d.pairings != nil && d.peers.HasLocal() {
			ch := make(chan bool, 1)
			d.pairings.mu.Lock()
			d.pairings.waiting[who.TokenName] = ch
			d.pairings.mu.Unlock()
			defer func() {
				d.pairings.mu.Lock()
				delete(d.pairings.waiting, who.TokenName)
				d.pairings.mu.Unlock()
			}()
			_ = ipc.SendEvent(protocol.EventPairingPending, "", "", "", struct{}{})
			d.peers.BroadcastLocal(protocol.EventPairingRequest, protocol.PairingRequestData{
				Credential: who.TokenName, Name: who.Name, DeviceType: who.DeviceType, IP: ip,
			})
			select {
			case allow := <-ch:
				if !allow {
					_, _ = d.settings.RemoveToken(who.TokenName)
					return errors.New("pairing was declined")
				}
			case <-time.After(pairingApprovalWait):
				_, _ = d.settings.RemoveToken(who.TokenName)
				return errors.New("nobody approved the pairing in time")
			}
		}
	}
	return d.settings.ClaimPairing(who.TokenName, who.DeviceID)
}
