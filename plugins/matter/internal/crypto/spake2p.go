package crypto

import (
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
)

// SPAKE2+ over P-256, as Matter's PASE uses it. Both sides prove they know
// the same passcode without ever sending it, and end up with the session
// keys. The device stores only a verifier, so a stolen device does not leak
// a passcode that would let an attacker commission other devices.
//
// Roles follow the specification: the Prover (Stulp, the commissioner) knows
// the passcode; the Verifier (the device being commissioned) knows only the
// registration record derived from it.

const (
	// pointBytes is an uncompressed P-256 point: 0x04 || X || Y.
	pointBytes = 65
	// scalarBytes is the width of a P-256 scalar.
	scalarBytes = 32
	// pbkdfOutputBytes is the per-scalar PBKDF2 output before reduction:
	// the group size plus 8 octets, so the reduction is unbiased.
	pbkdfOutputBytes = scalarBytes + 8
)

// The SPAKE2+ constants M and N for P-256, in compressed form, as fixed by
// the Matter specification. decodePoint verifies they are on the curve, so a
// corrupted constant fails loudly at startup instead of silently weakening
// the exchange.
var (
	pointM = mustDecodePoint("02886e2f97ace46e55ba9dd7242579f2993b64e16ef3dcab95afd497333d8fa12f")
	pointN = mustDecodePoint("03d8bbd6c639c62937b04d997f38c3770719c629d7014d49a24b4f98baa1292b49")
)

// Info strings from the Matter specification's key schedule.
var (
	infoConfirmationKeys = []byte("ConfirmationKeys")
	infoSessionKeys      = []byte("SessionKeys")
	// ContextPrefix starts the PASE transcript context.
	ContextPrefix = []byte("CHIP PAKE V1 Commissioning")
)

// PBKDF parameter bounds from the specification.
const (
	MinSaltBytes  = 16
	MaxSaltBytes  = 32
	MinIterations = 1000
	MaxIterations = 100000
)

// Scalars are the SPAKE2+ secrets a passcode expands into. The commissioner
// needs both; the device only ever stores what Registration holds.
type Scalars struct {
	W0 *big.Int
	W1 *big.Int
}

// Registration is what a device stores for a passcode: w0 plus the public
// point L = w1*P. It is deliberately not enough to impersonate the
// commissioner.
type Registration struct {
	W0 *big.Int
	L  []byte // uncompressed P-256 point
}

// SessionKeys are the outputs of a completed PASE exchange.
type SessionKeys struct {
	// I2R encrypts messages from the initiator to the responder.
	I2R []byte
	// R2I encrypts messages the other way.
	R2I []byte
	// AttestationChallenge signs the device attestation during
	// commissioning.
	AttestationChallenge []byte
}

// DeriveScalars expands a passcode into the SPAKE2+ scalars w0 and w1 using
// PBKDF2-HMAC-SHA256, with the salt and iteration count the device names in
// its PBKDFParamResponse.
func DeriveScalars(passcode uint32, salt []byte, iterations int) (Scalars, error) {
	if len(salt) < MinSaltBytes || len(salt) > MaxSaltBytes {
		return Scalars{}, fmt.Errorf("PBKDF salt must be %d..%d bytes, got %d", MinSaltBytes, MaxSaltBytes, len(salt))
	}
	if iterations < MinIterations || iterations > MaxIterations {
		return Scalars{}, fmt.Errorf("PBKDF iteration count %d is outside %d..%d", iterations, MinIterations, MaxIterations)
	}
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], passcode)
	derived, err := pbkdf2.Key(sha256.New, string(encoded[:]), salt, iterations, 2*pbkdfOutputBytes)
	if err != nil {
		return Scalars{}, fmt.Errorf("derive PAKE scalars: %w", err)
	}
	order := elliptic.P256().Params().N
	return Scalars{
		W0: new(big.Int).Mod(new(big.Int).SetBytes(derived[:pbkdfOutputBytes]), order),
		W1: new(big.Int).Mod(new(big.Int).SetBytes(derived[pbkdfOutputBytes:]), order),
	}, nil
}

// Register turns scalars into the record a device stores.
func (s Scalars) Register() Registration {
	return Registration{W0: s.W0, L: scalarBaseMult(s.W1).bytes()}
}

// Context builds the PASE transcript context: the fixed prefix followed by
// the two PBKDF parameter messages exactly as they went over the wire. Both
// sides must hash identical bytes, which is what binds the exchange to this
// specific negotiation.
func Context(pbkdfParamRequest, pbkdfParamResponse []byte) []byte {
	hash := sha256.New()
	hash.Write(ContextPrefix)
	hash.Write(pbkdfParamRequest)
	hash.Write(pbkdfParamResponse)
	return hash.Sum(nil)
}

// Prover is the commissioner side of PASE. It knows the passcode.
type Prover struct {
	context []byte
	scalars Scalars
	x       *big.Int
	share   []byte // X, sent as Pake1

	keys       *SessionKeys
	expectedCA []byte
	expectedCB []byte
}

// NewProver starts an exchange and produces the Pake1 share X.
func NewProver(context []byte, scalars Scalars) (*Prover, error) {
	if scalars.W0 == nil || scalars.W1 == nil {
		return nil, errors.New("SPAKE2+ prover needs both w0 and w1")
	}
	secret, err := randomScalar()
	if err != nil {
		return nil, err
	}
	// X = x*P + w0*M
	share := add(scalarBaseMult(secret), scalarMult(pointM, scalars.W0))
	return &Prover{context: context, scalars: scalars, x: secret, share: share.bytes()}, nil
}

// Share returns X, the Pake1 payload.
func (p *Prover) Share() []byte { return p.share }

// Finish consumes the device's Pake2 (its share Y and confirmation cB) and
// returns the Pake3 confirmation cA plus the session keys. A returned error
// means the device did not know the passcode.
func (p *Prover) Finish(deviceShare, deviceConfirmation []byte) ([]byte, *SessionKeys, error) {
	share, err := decodePoint(deviceShare)
	if err != nil {
		return nil, nil, fmt.Errorf("device share Y: %w", err)
	}
	// The device's share minus its mask: Y - w0*N == y*P.
	unmasked := add(share, negate(scalarMult(pointN, p.scalars.W0)))
	if unmasked.isIdentity() {
		return nil, nil, errors.New("device share Y unmasks to the identity element")
	}
	secretZ := scalarMult(unmasked, p.x)
	secretV := scalarMult(unmasked, p.scalars.W1)

	keys, confirmA, confirmB := schedule(p.context, p.share, deviceShare,
		secretZ.bytes(), secretV.bytes(), p.scalars.W0)
	if subtle.ConstantTimeCompare(confirmB, deviceConfirmation) != 1 {
		return nil, nil, errors.New("device confirmation is invalid: the passcode does not match")
	}
	p.keys, p.expectedCA, p.expectedCB = keys, confirmA, confirmB
	return confirmA, keys, nil
}

// Verifier is the device side of PASE. It only knows the registration
// record, never the passcode.
type Verifier struct {
	context      []byte
	registration Registration
	share        []byte // Y, sent as Pake2

	keys     *SessionKeys
	confirmA []byte
}

// NewVerifier starts the device side.
func NewVerifier(context []byte, registration Registration) (*Verifier, error) {
	if registration.W0 == nil {
		return nil, errors.New("SPAKE2+ verifier needs w0")
	}
	if _, err := decodePoint(registration.L); err != nil {
		return nil, fmt.Errorf("registration point L: %w", err)
	}
	return &Verifier{context: context, registration: registration}, nil
}

// Accept consumes the commissioner's Pake1 share X and returns the Pake2
// payload: this side's share Y and the confirmation cB.
func (v *Verifier) Accept(proverShare []byte) (share []byte, confirmation []byte, err error) {
	commissionerShare, err := decodePoint(proverShare)
	if err != nil {
		return nil, nil, fmt.Errorf("commissioner share X: %w", err)
	}
	secret, err := randomScalar()
	if err != nil {
		return nil, nil, err
	}
	// Y = y*P + w0*N
	own := add(scalarBaseMult(secret), scalarMult(pointN, v.registration.W0))
	// X - w0*M == x*P
	unmasked := add(commissionerShare, negate(scalarMult(pointM, v.registration.W0)))
	if unmasked.isIdentity() {
		return nil, nil, errors.New("commissioner share X unmasks to the identity element")
	}
	secretZ := scalarMult(unmasked, secret)

	verifierPoint, err := decodePoint(v.registration.L)
	if err != nil {
		return nil, nil, err
	}
	secretV := scalarMult(verifierPoint, secret)

	v.share = own.bytes()
	keys, confirmA, confirmB := schedule(v.context, proverShare, v.share,
		secretZ.bytes(), secretV.bytes(), v.registration.W0)
	v.keys, v.confirmA = keys, confirmA
	return v.share, confirmB, nil
}

// Confirm checks the commissioner's Pake3 confirmation cA and releases the
// session keys.
func (v *Verifier) Confirm(proverConfirmation []byte) (*SessionKeys, error) {
	if v.keys == nil {
		return nil, errors.New("Accept must run before Confirm")
	}
	if subtle.ConstantTimeCompare(v.confirmA, proverConfirmation) != 1 {
		return nil, errors.New("commissioner confirmation is invalid: the passcode does not match")
	}
	return v.keys, nil
}

// schedule derives the shared keys from the transcript. Both roles run this
// with identical inputs, which is exactly what the confirmations prove.
func schedule(context, shareX, shareY, secretZ, secretV []byte, w0 *big.Int) (*SessionKeys, []byte, []byte) {
	transcript := buildTranscript(context, shareX, shareY, secretZ, secretV, w0)
	main := sha256.Sum256(transcript)
	keyKa, keyKe := main[:16], main[16:]

	confirmationKeys, err := hkdf.Key(sha256.New, keyKa, nil, string(infoConfirmationKeys), 32)
	if err != nil {
		// HKDF only fails on absurd output lengths, which are constants here.
		panic("matter/crypto: deriving SPAKE2+ confirmation keys: " + err.Error())
	}
	sessionKeys, err := hkdf.Key(sha256.New, keyKe, nil, string(infoSessionKeys), 48)
	if err != nil {
		panic("matter/crypto: deriving SPAKE2+ session keys: " + err.Error())
	}

	confirmA := macWith(confirmationKeys[:16], shareY)
	confirmB := macWith(confirmationKeys[16:], shareX)
	return &SessionKeys{
		I2R:                  sessionKeys[:16],
		R2I:                  sessionKeys[16:32],
		AttestationChallenge: sessionKeys[32:],
	}, confirmA, confirmB
}

func macWith(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// buildTranscript assembles TT: every element prefixed with its length as a
// 64-bit little-endian count, so no two different transcripts can collide.
func buildTranscript(context, shareX, shareY, secretZ, secretV []byte, w0 *big.Int) []byte {
	var transcript []byte
	appendItem := func(item []byte) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(item)))
		transcript = append(transcript, length[:]...)
		transcript = append(transcript, item...)
	}
	appendItem(context)
	appendItem(nil) // identity A: empty in Matter
	appendItem(nil) // identity B: empty in Matter
	appendItem(pointM.bytes())
	appendItem(pointN.bytes())
	appendItem(shareX)
	appendItem(shareY)
	appendItem(secretZ)
	appendItem(secretV)
	appendItem(w0.FillBytes(make([]byte, scalarBytes)))
	return transcript
}

// point is an affine P-256 point.
type point struct{ x, y *big.Int }

func (p point) bytes() []byte {
	return elliptic.Marshal(elliptic.P256(), p.x, p.y) //nolint:staticcheck // no stdlib replacement exposes point addition
}

// isIdentity reports the point at infinity, which Go represents as (0,0).
// A share that unmasks to it would collapse the shared secret.
func (p point) isIdentity() bool { return p.x.Sign() == 0 && p.y.Sign() == 0 }

// decodePoint accepts compressed or uncompressed encodings and rejects
// anything that is not a finite point on P-256. Validating peer-supplied
// points is required: an off-curve point would leak key material.
func decodePoint(encoded []byte) (point, error) {
	if len(encoded) == 0 {
		return point{}, errors.New("point is empty")
	}
	var x, y *big.Int
	switch encoded[0] {
	case 0x02, 0x03:
		x, y = elliptic.UnmarshalCompressed(elliptic.P256(), encoded)
	case 0x04:
		x, y = elliptic.Unmarshal(elliptic.P256(), encoded) //nolint:staticcheck // matches Marshal above
	default:
		return point{}, fmt.Errorf("unsupported point encoding 0x%02x", encoded[0])
	}
	if x == nil || y == nil {
		return point{}, errors.New("point is not on the P-256 curve")
	}
	return point{x, y}, nil
}

func mustDecodePoint(compressed string) point {
	raw, err := hex.DecodeString(compressed)
	if err != nil {
		panic("matter/crypto: invalid SPAKE2+ constant: " + err.Error())
	}
	value, err := decodePoint(raw)
	if err != nil {
		panic("matter/crypto: invalid SPAKE2+ constant: " + err.Error())
	}
	return value
}

func scalarBaseMult(k *big.Int) point {
	x, y := elliptic.P256().ScalarBaseMult(k.FillBytes(make([]byte, scalarBytes))) //nolint:staticcheck
	return point{x, y}
}

func scalarMult(p point, k *big.Int) point {
	x, y := elliptic.P256().ScalarMult(p.x, p.y, k.FillBytes(make([]byte, scalarBytes))) //nolint:staticcheck
	return point{x, y}
}

func add(a, b point) point {
	x, y := elliptic.P256().Add(a.x, a.y, b.x, b.y) //nolint:staticcheck
	return point{x, y}
}

// negate mirrors a point across the x-axis, turning addition into
// subtraction.
func negate(p point) point {
	return point{
		x: new(big.Int).Set(p.x),
		y: new(big.Int).Sub(elliptic.P256().Params().P, p.y),
	}
}

func randomScalar() (*big.Int, error) {
	order := elliptic.P256().Params().N
	for {
		candidate, err := rand.Int(rand.Reader, order)
		if err != nil {
			return nil, fmt.Errorf("generate SPAKE2+ scalar: %w", err)
		}
		if candidate.Sign() != 0 {
			return candidate, nil
		}
	}
}
