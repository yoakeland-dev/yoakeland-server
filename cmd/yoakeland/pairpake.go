package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"time"

	"yoakeland/internal/cpace"
	"yoakeland/internal/protocol"
)

// Pairing with a PIN (SPEC.md §2a). The client opens a connection, sends an `auth` frame whose
// `pake` field holds its CPace share instead of a token, and the two sides derive the same key only
// if both know the PIN. The client then proves it with a MAC, and the server answers with a new
// device credential sealed under that key. The credential is an ordinary one-device pairing token,
// so everything after this connection is the existing sign-in. Nothing secret ever crosses the wire
// in the clear, and an impostor server learns nothing it can test a guess against.
//
// The CPace channel identifier carries a TLS exporter on a raw TLS connection, which ties the
// exchange to that one TLS session: a man in the middle who terminates TLS himself is on a different
// session on each side, so his two halves derive different keys and the exchange fails. (A browser
// on a WebSocket or WebRTC transport cannot read an exporter; those transports are authenticated by
// the CA or the pinned fingerprint instead, so they bind to nothing.)
const (
	pakeLabel    = "yoake/pair/v1"
	pakeExporter = "EXPORTER-yoake-pair-v1"
	shareBytes   = 32
)

var errPairFailed = errors.New("pairing failed")

// pakeCI is the CPace channel identifier: the protocol's own name, and the TLS binding when there is one.
func pakeCI(binding []byte) []byte { return cpace.LVCat([]byte(pakeLabel), binding) }

// channelBinding is the TLS exporter for the connection under conn, or nil when it is not a raw TLS
// connection (a unix socket, a WebSocket, a WebRTC channel).
func channelBinding(conn net.Conn) []byte {
	if bc, ok := conn.(*bufferedConn); ok {
		conn = bc.Conn
	}
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return nil
	}
	state := tc.ConnectionState()
	b, err := state.ExportKeyingMaterial(pakeExporter, []byte(pakeLabel), 32)
	if err != nil {
		return nil
	}
	return b
}

// pakeKeys splits the ISK into the key the client's confirmation is MACed under and the key the
// credential is sealed under.
func pakeKeys(isk []byte) (confirm, seal []byte, err error) {
	if confirm, err = hkdf.Key(sha512.New, isk, nil, pakeLabel+" confirm", 32); err != nil {
		return nil, nil, err
	}
	seal, err = hkdf.Key(sha512.New, isk, nil, pakeLabel+" seal", 32)
	return confirm, seal, err
}

// pakeMac is the client's key confirmation. It names the session, so it can't be lifted from another.
func pakeMac(confirmKey, sid []byte) []byte {
	m := hmac.New(sha256.New, confirmKey)
	m.Write(cpace.LVCat([]byte(pakeLabel+" client"), sid))
	return m.Sum(nil)
}

func pakeSeal(sealKey, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(sealKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, []byte(pakeLabel+" result")), nil
}

// decodePakeStart checks the shapes: a session id of 16-64 bytes, a 32-byte share.
func decodePakeStart(p *protocol.PakeStart) (sid, ya []byte, err error) {
	if sid, err = base64.StdEncoding.DecodeString(p.SID); err != nil || len(sid) < 16 || len(sid) > 64 {
		return nil, nil, errPairFailed
	}
	if ya, err = base64.StdEncoding.DecodeString(p.Ya); err != nil || len(ya) != shareBytes {
		return nil, nil, errPairFailed
	}
	return sid, ya, nil
}

// pairWithPIN runs the server's half for one connection whose first frame carried who.pake. It
// returns nil only after sending the sealed credential; the caller closes the connection either
// way. Every refusal is the same errPairFailed: the reason (no PIN, a wrong one, too many tries)
// is nothing a guesser should learn.
func (d connDeps) pairWithPIN(ipc *protocol.IPCStream, conn net.Conn, who peerIdentity) error {
	if who.DeviceID == "" {
		return errors.New("this app is too old to pair; update it")
	}
	sid, ya, err := decodePakeStart(who.pake)
	if err != nil {
		return err
	}
	pin, err := d.pins.begin()
	if err != nil {
		return err
	}
	defer d.pins.settle(pin)

	sess, err := cpace.Start([]byte(pin.value), pakeCI(channelBinding(conn)), sid, nil)
	if err != nil {
		return errPairFailed
	}
	isk, err := sess.FinishResponder(ya, []byte(who.DeviceID))
	if err != nil {
		return errPairFailed
	}
	if err := ipc.SendEvent(protocol.EventPakeReply, "", "", "", protocol.PakeReplyData{Yb: base64.StdEncoding.EncodeToString(sess.Message())}); err != nil {
		return errPairFailed
	}

	_ = conn.SetReadDeadline(time.Now().Add(preAuthTimeout))
	env, err := ipc.ReadEnvelope()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || env.Type != protocol.EventPakeConfirm {
		return errPairFailed
	}
	var confirm protocol.PakeConfirmData
	if json.Unmarshal(env.Data, &confirm) != nil {
		return errPairFailed
	}
	mac, macErr := base64.StdEncoding.DecodeString(confirm.Mac)
	confirmKey, sealKey, err := pakeKeys(isk)
	if err != nil || macErr != nil || !hmac.Equal(mac, pakeMac(confirmKey, sid)) {
		return errPairFailed // a wrong PIN, or a man in the middle on the TLS session
	}
	if !d.pins.spend(pin) {
		return errPairFailed
	}

	// Make the credential: an ordinary one-device pairing token, bound to this device before it is
	// ever returned. If someone is at the desktop to say yes, they are asked, as for a QR code.
	token, err := generateAuthToken()
	if err != nil {
		return errPairFailed
	}
	suffix, err := generateAuthToken()
	if err != nil {
		return errPairFailed
	}
	name := "pin-" + suffix[:6]
	if err := d.settings.AddPairingToken(name, token, ""); err != nil {
		return errPairFailed
	}
	_, rec, _ := d.settings.LookupToken(token)
	who.TokenName = name
	if err := d.admitPairing(ipc, who, rec, remoteIP(conn, "tcp")); err != nil {
		return err
	}

	result := struct {
		Token string `json:"token"`
		CAPem string `json:"caPem,omitempty"`
	}{Token: token}
	if d.remote != nil {
		result.CAPem, _ = d.remote.CAPEM()
	}
	plain, _ := json.Marshal(result)
	sealed, err := pakeSeal(sealKey, plain)
	if err != nil {
		return errPairFailed
	}
	return ipc.SendEvent(protocol.EventPakeResult, "", "", "", protocol.PakeResultData{Sealed: base64.StdEncoding.EncodeToString(sealed)})
}
