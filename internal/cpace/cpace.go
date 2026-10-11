// Package cpace is CPace, the balanced password-authenticated key exchange, in the one cipher suite
// yoakeland uses: CPACE-X25519-SHA512 of draft-irtf-cfrg-cpace-21, initiator/responder flavour.
// Two parties that hold the same low-entropy secret (PRS, here a typed pairing PIN) and the same
// channel identifier (CI) and session id (sid) end up with the same ISK; anyone else gets nothing
// to test a guess against offline, so a short PIN is enough, with online attempts counted by the
// caller.
//
// It is checked against the draft's own test vectors (cpace_test.go), and src/cpace.js in the
// desktop app implements the same thing against the same vectors.
//
// ponytail: the Elligator 2 step uses math/big, which is not constant time. It runs once per
// attempt on a hash of the PIN, and the caller allows a handful of attempts per PIN, so there is
// no oracle to time. Swap in a constant-time field (filippo.io/edwards25519/field) if that stops
// being true.
package cpace

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"math/big"
)

const (
	dsi         = "CPace255"
	dsiISK      = "CPace255_ISK"
	fieldBytes  = 32
	hashBlock   = 128 // SHA-512's input block, for the generator string's padding
	elligatorA  = 486662
	elligatorZ  = 2
	scalarBytes = 32
)

// ErrAbort is returned when the other side's message is invalid or a low-order point: the exchange
// must stop, and nothing derived from it is to be used.
var ErrAbort = errors.New("cpace: aborted (invalid or low-order point)")

var (
	p = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	a = big.NewInt(elligatorA)
)

// PrependLen is prepend_len of the draft: a LEB128 length, then the bytes.
func PrependLen(b []byte) []byte {
	var out []byte
	n := len(b)
	for {
		if n < 128 {
			out = append(out, byte(n))
		} else {
			out = append(out, byte(n&0x7f)+0x80)
		}
		n >>= 7
		if n == 0 {
			break
		}
	}
	return append(out, b...)
}

// LVCat is lv_cat of the draft: every argument with its length in front.
func LVCat(parts ...[]byte) []byte {
	var out []byte
	for _, b := range parts {
		out = append(out, PrependLen(b)...)
	}
	return out
}

func generatorString(prs, ci, sid []byte) []byte {
	zpad := hashBlock - 1 - len(PrependLen(prs)) - len(PrependLen([]byte(dsi)))
	if zpad < 0 {
		zpad = 0
	}
	return LVCat([]byte(dsi), prs, make([]byte, zpad), ci, sid)
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func fieldInv(x *big.Int) *big.Int { return new(big.Int).ModInverse(x, p) }

// elligator2 maps a field element to the u coordinate of a Curve25519 point (appendix A.5).
func elligator2(r *big.Int) []byte {
	mod := func(x *big.Int) *big.Int { return x.Mod(x, p) }
	// v = -A / (1 + Z r^2)
	den := mod(new(big.Int).Add(big.NewInt(1), new(big.Int).Mul(big.NewInt(elligatorZ), new(big.Int).Mul(r, r))))
	v := mod(new(big.Int).Neg(new(big.Int).Mul(a, fieldInv(den))))
	// epsilon = legendre(v^3 + A v^2 + v)
	v2 := mod(new(big.Int).Mul(v, v))
	t := mod(new(big.Int).Add(new(big.Int).Add(new(big.Int).Mul(v2, v), new(big.Int).Mul(a, v2)), v))
	eps := big.Jacobi(t, p)
	// x = epsilon * v - (1 - epsilon) * A/2
	halfA := new(big.Int).Rsh(a, 1) // A is even
	x := new(big.Int).Mul(big.NewInt(int64(eps)), v)
	x.Sub(x, new(big.Int).Mul(big.NewInt(int64(1-eps)), halfA))
	mod(x)
	out := make([]byte, fieldBytes)
	return reverse(x.FillBytes(out))
}

// calculateGenerator is G_X25519.calculate_generator: the point both sides derive from the secret.
func calculateGenerator(prs, ci, sid []byte) []byte {
	h := sha512.Sum512(generatorString(prs, ci, sid))
	u := append([]byte(nil), h[:fieldBytes]...)
	u[fieldBytes-1] &= 0x7f // decodeUCoordinate ignores bit 255
	return elligator2(new(big.Int).SetBytes(reverse(u)))
}

// scalarMultVfy is G_X25519.scalar_mult_vfy: X25519, with a neutral (all zero) result an abort.
func scalarMultVfy(scalar, point []byte) ([]byte, error) {
	if len(scalar) != scalarBytes || len(point) != fieldBytes {
		return nil, ErrAbort
	}
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, ErrAbort
	}
	pub, err := ecdh.X25519().NewPublicKey(point)
	if err != nil {
		return nil, ErrAbort
	}
	out, err := priv.ECDH(pub) // errors on a low-order point, i.e. an all-zero result
	if err != nil {
		return nil, ErrAbort
	}
	return out, nil
}

// Session is one party's half of an exchange between Start and Finish*.
type Session struct {
	scalar []byte
	sid    []byte
	ad     []byte // this party's own associated data
	msg    []byte // this party's public share Y
}

// Start begins an exchange as either party: prs is the shared secret, ci the channel identifier,
// sid a session id unique to this run, ad this party's associated data (carried in the
// transcript, so the other side authenticates it).
func Start(prs, ci, sid, ad []byte) (*Session, error) {
	scalar := make([]byte, scalarBytes)
	if _, err := rand.Read(scalar); err != nil {
		return nil, err
	}
	return start(prs, ci, sid, ad, scalar)
}

func start(prs, ci, sid, ad, scalar []byte) (*Session, error) {
	y, err := scalarMultVfy(scalar, calculateGenerator(prs, ci, sid))
	if err != nil {
		return nil, err
	}
	return &Session{scalar: scalar, sid: sid, ad: ad, msg: y}, nil
}

// Message is the public share to send to the other party.
func (s *Session) Message() []byte { return append([]byte(nil), s.msg...) }

func (s *Session) isk(peerMsg, ya, ada, yb, adb []byte) ([]byte, error) {
	k, err := scalarMultVfy(s.scalar, peerMsg)
	if err != nil {
		return nil, err
	}
	transcript := append(LVCat(ya, ada), LVCat(yb, adb)...)
	h := sha512.Sum512(append(LVCat([]byte(dsiISK), s.sid, k), transcript...))
	return h[:], nil
}

// FinishInitiator completes the exchange for the party that sent first, given the responder's
// share and associated data. The 64-byte ISK matches the responder's iff PRS, CI and sid match and
// both saw the same messages.
func (s *Session) FinishInitiator(yb, adb []byte) ([]byte, error) {
	return s.isk(yb, s.msg, s.ad, yb, adb)
}

// FinishResponder completes the exchange for the party that answered, given the initiator's share
// and associated data.
func (s *Session) FinishResponder(ya, ada []byte) ([]byte, error) {
	return s.isk(ya, ya, ada, s.msg, s.ad)
}
