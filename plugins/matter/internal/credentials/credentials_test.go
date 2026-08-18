package credentials

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func TestFabricCreatesVerifiableOperationalCertificates(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fabric, err := NewFabric(0x1234, 0xCA, 0x112233, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := fabric.Validate(); err != nil {
		t.Fatal(err)
	}
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceNOC, err := fabric.SignNode(&deviceKey.PublicKey, 0x445566, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deviceNOC.CheckSignatureFrom(fabric.RootCertificate); err != nil {
		t.Fatalf("device NOC chain: %v", err)
	}
	if !deviceNOC.PublicKey.(*ecdsa.PublicKey).Equal(&deviceKey.PublicKey) {
		t.Fatal("device NOC does not contain CSR public key")
	}

	for name, testCase := range map[string]struct {
		certificate *x509.Certificate
		issuer      *ecdsa.PublicKey
	}{
		"root":       {fabric.RootCertificate, &fabric.RootKey.PublicKey},
		"controller": {fabric.ControllerNOC, &fabric.RootKey.PublicKey},
		"device":     {deviceNOC, &fabric.RootKey.PublicKey},
	} {
		encoded, err := MatterCertificate(testCase.certificate)
		if err != nil {
			t.Fatalf("%s compact certificate: %v", name, err)
		}
		root, err := tlv.NewReader(encoded).Next()
		if err != nil || root.Type != tlv.TypeStructure {
			t.Fatalf("%s compact certificate is not TLV: type=%d err=%v", name, root.Type, err)
		}
		if len(encoded) > 400 {
			t.Fatalf("%s compact certificate exceeds Matter's 400-byte limit: %d", name, len(encoded))
		}
		reconstructed, err := reconstructMatterTBSCertificate(encoded)
		if err != nil {
			t.Fatalf("%s reconstruct TBS: %v", name, err)
		}
		if !bytes.Equal(reconstructed, testCase.certificate.RawTBSCertificate) {
			t.Fatalf("%s reconstructed TBS differs from the exact signed DER\noriginal: % X\nrebuilt:  % X",
				name, testCase.certificate.RawTBSCertificate, reconstructed)
		}
		if err := verifyMatterCertificateSignature(encoded, testCase.issuer); err != nil {
			t.Fatalf("%s reconstructed signature: %v", name, err)
		}
	}
}

func TestCompactCertificateSignatureDetectsMutation(t *testing.T) {
	fabric, err := NewFabric(1, 2, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := fabric.ControllerMatterCertificate()
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-2] ^= 1 // last signature byte sits immediately before the structure end
	if err := verifyMatterCertificateSignature(encoded, &fabric.RootKey.PublicKey); err == nil {
		t.Fatal("mutated compact certificate signature was accepted")
	}
}

func TestFabricPersistenceRoundTrip(t *testing.T) {
	fabric, err := NewFabric(5, 6, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalPrivateKey(fabric.RootKey)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParsePrivateKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Equal(fabric.RootKey) {
		t.Fatal("PKCS#8 round-trip changed root key")
	}
	left, err := fabric.CompressedID()
	if err != nil {
		t.Fatal(err)
	}
	fabric.RootKey = reloaded
	right, err := fabric.CompressedID()
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("compressed fabric ID changed after key reload")
	}
}

func TestDestinationIDIsBoundToPeerAndRandom(t *testing.T) {
	fabric, err := NewFabric(1, 2, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	randomValue := make([]byte, 32)
	one, err := fabric.DestinationID(randomValue, 4)
	if err != nil {
		t.Fatal(err)
	}
	two, _ := fabric.DestinationID(randomValue, 5)
	if string(one) == string(two) {
		t.Fatal("destination ID is not bound to peer node ID")
	}
	randomValue[0] = 1
	three, _ := fabric.DestinationID(randomValue, 4)
	if string(one) == string(three) {
		t.Fatal("destination ID is not bound to initiator random")
	}
}
