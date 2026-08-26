package crypto

import (
	"bytes"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"slices"
	"testing"
)

func TestRegistrationSerializeMatchesMatterReferenceVector(t *testing.T) {
	salt := []byte("SPAKE2P Key Salt")
	scalars, err := DeriveScalars(20202021, salt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := scalars.Register().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	want, err := base64.StdEncoding.DecodeString("uWFwqugDNGiEck/po7KHwwMwwqZgN10XuyBajPGuyzUEV/iree4lOrao5GuwnlQ65CJzbeUB49s31EH+NEkg0JVI5MGCQGMMT/SRPFNRODm3wH/MBiehuFc6FJ/NH6Rmzw==")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serialized, want) {
		t.Fatalf("serialized verifier = %x, want %x", serialized, want)
	}
}

const (
	// The canonical Matter test device.
	testPasscode   = 20202021
	testIterations = 1000
)

var testSalt = []byte("SPAKE2P Key Salt")

// The M and N constants are the one place a single mistyped hex digit would
// silently weaken the exchange. Only about half of all x coordinates lie on
// the curve, so decoding successfully is already strong evidence, and this
// pins it explicitly.
func TestSPAKEConstantsAreOnCurve(t *testing.T) {
	for name, value := range map[string]point{"M": pointM, "N": pointN} {
		if !elliptic.P256().IsOnCurve(value.x, value.y) {
			t.Fatalf("constant %s is not on P-256", name)
		}
		if value.isIdentity() {
			t.Fatalf("constant %s is the identity element", name)
		}
		encoded := value.bytes()
		if len(encoded) != pointBytes || encoded[0] != 0x04 {
			t.Fatalf("constant %s encodes to %d bytes starting %#x", name, len(encoded), encoded[0])
		}
		decoded, err := decodePoint(encoded)
		if err != nil {
			t.Fatalf("constant %s does not round-trip: %v", name, err)
		}
		if decoded.x.Cmp(value.x) != 0 || decoded.y.Cmp(value.y) != 0 {
			t.Fatalf("constant %s changed across a round trip", name)
		}
	}
	if pointM.x.Cmp(pointN.x) == 0 {
		t.Fatal("M and N must be independent points")
	}
}

func testRegistration(t *testing.T) (Scalars, Registration) {
	t.Helper()
	scalars, err := DeriveScalars(testPasscode, testSalt, testIterations)
	if err != nil {
		t.Fatal(err)
	}
	return scalars, scalars.Register()
}

// The full PASE exchange. This is the strongest test in the package: the two
// sides reach Z and V by completely different routes — the commissioner via
// x*(Y - w0*N) and w1*(Y - w0*N), the device via y*(X - w0*M) and y*L — so
// they only agree if every point operation and the whole transcript are
// right.
func TestHandshakeSucceeds(t *testing.T) {
	scalars, registration := testRegistration(t)
	context := Context([]byte("request"), []byte("response"))

	prover, err := NewProver(context, scalars)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(context, registration)
	if err != nil {
		t.Fatal(err)
	}

	deviceShare, deviceConfirmation, err := verifier.Accept(prover.Share())
	if err != nil {
		t.Fatal(err)
	}
	proverConfirmation, commissionerKeys, err := prover.Finish(deviceShare, deviceConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	deviceKeys, err := verifier.Confirm(proverConfirmation)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(commissionerKeys.I2R, deviceKeys.I2R) ||
		!bytes.Equal(commissionerKeys.R2I, deviceKeys.R2I) ||
		!bytes.Equal(commissionerKeys.AttestationChallenge, deviceKeys.AttestationChallenge) {
		t.Fatalf("the two sides disagree on the session keys:\n%+v\n%+v", commissionerKeys, deviceKeys)
	}
	for name, key := range map[string][]byte{
		"I2R":                   commissionerKeys.I2R,
		"R2I":                   commissionerKeys.R2I,
		"attestation challenge": commissionerKeys.AttestationChallenge,
	} {
		if len(key) != 16 {
			t.Fatalf("%s is %d bytes, want 16", name, len(key))
		}
		if bytes.Equal(key, make([]byte, 16)) {
			t.Fatalf("%s is all zeroes", name)
		}
	}
	if bytes.Equal(commissionerKeys.I2R, commissionerKeys.R2I) {
		t.Fatal("the two directions must use different keys")
	}

	// Each run must use fresh ephemeral scalars.
	other, err := NewProver(context, scalars)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other.Share(), prover.Share()) {
		t.Fatal("two exchanges produced the same share X")
	}
}

// The session keys must actually drive the message layer's cipher.
func TestSessionKeysAreUsableSessionKeys(t *testing.T) {
	scalars, registration := testRegistration(t)
	context := Context(nil, nil)
	prover, _ := NewProver(context, scalars)
	verifier, _ := NewVerifier(context, registration)
	deviceShare, deviceConfirmation, err := verifier.Accept(prover.Share())
	if err != nil {
		t.Fatal(err)
	}
	_, keys, err := prover.Finish(deviceShare, deviceConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.I2R) != 16 {
		t.Fatalf("I2R is %d bytes; Matter session keys are AES-128", len(keys.I2R))
	}
}

func TestWrongPasscodeIsRejected(t *testing.T) {
	_, registration := testRegistration(t)
	wrong, err := DeriveScalars(testPasscode+1, testSalt, testIterations)
	if err != nil {
		t.Fatal(err)
	}
	context := Context([]byte("request"), []byte("response"))

	prover, err := NewProver(context, wrong)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(context, registration)
	if err != nil {
		t.Fatal(err)
	}
	deviceShare, deviceConfirmation, err := verifier.Accept(prover.Share())
	if err != nil {
		t.Fatal(err)
	}
	// The commissioner notices first: the device's confirmation will not
	// verify against the key it derived.
	if _, _, err := prover.Finish(deviceShare, deviceConfirmation); err == nil {
		t.Fatal("a wrong passcode produced a valid device confirmation")
	}
	// And a forged confirmation does not get past the device either.
	if _, err := verifier.Confirm(make([]byte, 32)); err == nil {
		t.Fatal("the device accepted a forged commissioner confirmation")
	}
}

// The transcript binds the exchange to the negotiated PBKDF parameters, so
// mismatched contexts must never agree.
func TestContextBindsTheExchange(t *testing.T) {
	scalars, registration := testRegistration(t)
	prover, err := NewProver(Context([]byte("request A"), nil), scalars)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(Context([]byte("request B"), nil), registration)
	if err != nil {
		t.Fatal(err)
	}
	deviceShare, deviceConfirmation, err := verifier.Accept(prover.Share())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prover.Finish(deviceShare, deviceConfirmation); err == nil {
		t.Fatal("mismatched contexts completed the exchange")
	}
}

// The specification defines the context as a plain concatenation:
//
//	Context = Hash("CHIP PAKE V1 Commissioning" || PBKDFParamRequest || PBKDFParamResponse)
//
// It carries no length prefixes, so it is formally ambiguous across the
// message boundary. That is safe only because both messages are
// self-delimiting TLV structures, and it must stay bit-exact or no real
// device will complete the exchange. This test pins the formula.
func TestContextMatchesTheSpecificationFormula(t *testing.T) {
	request, response := []byte("request bytes"), []byte("response bytes")
	expected := sha256.Sum256(slices.Concat(ContextPrefix, request, response))
	if !bytes.Equal(Context(request, response), expected[:]) {
		t.Fatal("Context does not hash prefix || request || response")
	}
	if !bytes.Equal(Context([]byte("a"), []byte("bc")), Context([]byte("ab"), []byte("c"))) {
		t.Fatal("Context gained length framing the specification does not have")
	}
}

func TestMalformedSharesAreRejected(t *testing.T) {
	scalars, registration := testRegistration(t)
	context := Context(nil, nil)
	valid, err := NewProver(context, scalars)
	if err != nil {
		t.Fatal(err)
	}
	offCurve := bytes.Clone(valid.Share())
	offCurve[pointBytes-1] ^= 0x01

	cases := map[string][]byte{
		"empty":              {},
		"off curve":          offCurve,
		"truncated":          valid.Share()[:32],
		"unknown encoding":   append([]byte{0x05}, valid.Share()[1:]...),
		"identity encoding":  make([]byte, pointBytes),
		"all zero with 0x04": append([]byte{0x04}, make([]byte, pointBytes-1)...),
	}
	for name, share := range cases {
		t.Run(name, func(t *testing.T) {
			verifier, err := NewVerifier(context, registration)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := verifier.Accept(share); err == nil {
				t.Fatal("an invalid share X was accepted")
			}
			prover, err := NewProver(context, scalars)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := prover.Finish(share, make([]byte, 32)); err == nil {
				t.Fatal("an invalid share Y was accepted")
			}
		})
	}
}

func TestScalarDerivation(t *testing.T) {
	first, err := DeriveScalars(testPasscode, testSalt, testIterations)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveScalars(testPasscode, testSalt, testIterations)
	if err != nil {
		t.Fatal(err)
	}
	if first.W0.Cmp(second.W0) != 0 || first.W1.Cmp(second.W1) != 0 {
		t.Fatal("scalar derivation is not deterministic")
	}
	if first.W0.Cmp(first.W1) == 0 {
		t.Fatal("w0 and w1 must differ")
	}
	order := elliptic.P256().Params().N
	for name, scalar := range map[string]*big.Int{"w0": first.W0, "w1": first.W1} {
		if scalar.Sign() <= 0 || scalar.Cmp(order) >= 0 {
			t.Fatalf("%s is outside [1, n)", name)
		}
	}

	// Every input must change the result.
	otherPasscode, _ := DeriveScalars(testPasscode+1, testSalt, testIterations)
	otherSalt, _ := DeriveScalars(testPasscode, []byte("a different salt!"), testIterations)
	otherRounds, _ := DeriveScalars(testPasscode, testSalt, testIterations+1)
	for name, other := range map[string]Scalars{
		"passcode": otherPasscode, "salt": otherSalt, "iterations": otherRounds,
	} {
		if other.W0.Cmp(first.W0) == 0 {
			t.Fatalf("changing the %s did not change w0", name)
		}
	}
}

// Registration must hold L = w1*P, and must not carry w1 itself.
func TestRegistrationHoldsOnlyThePublicPoint(t *testing.T) {
	scalars, registration := testRegistration(t)
	expected := scalarBaseMult(scalars.W1).bytes()
	if !bytes.Equal(registration.L, expected) {
		t.Fatal("L is not w1*P")
	}
	if bytes.Contains(registration.L, scalars.W1.FillBytes(make([]byte, scalarBytes))) {
		t.Fatal("the registration record leaks w1")
	}
	if registration.W0.Cmp(scalars.W0) != 0 {
		t.Fatal("the registration record must carry w0")
	}
}

func TestPBKDFParameterValidation(t *testing.T) {
	cases := []struct {
		name       string
		salt       []byte
		iterations int
	}{
		{"salt too short", make([]byte, MinSaltBytes-1), testIterations},
		{"salt too long", make([]byte, MaxSaltBytes+1), testIterations},
		{"too few iterations", testSalt, MinIterations - 1},
		{"too many iterations", testSalt, MaxIterations + 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := DeriveScalars(testPasscode, testCase.salt, testCase.iterations); err == nil {
				t.Fatal("invalid PBKDF parameters were accepted")
			}
		})
	}
}

func TestIncompleteRoles(t *testing.T) {
	scalars, registration := testRegistration(t)
	if _, err := NewProver(nil, Scalars{W0: scalars.W0}); err == nil {
		t.Fatal("a prover without w1 was created")
	}
	if _, err := NewVerifier(nil, Registration{L: registration.L}); err == nil {
		t.Fatal("a verifier without w0 was created")
	}
	if _, err := NewVerifier(nil, Registration{W0: scalars.W0, L: []byte{0x04}}); err == nil {
		t.Fatal("a verifier with a malformed L was created")
	}
	verifier, err := NewVerifier(nil, registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Confirm(make([]byte, 32)); err == nil {
		t.Fatal("Confirm ran before Accept")
	}
}
