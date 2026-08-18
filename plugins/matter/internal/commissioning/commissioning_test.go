package commissioning

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func TestNOCSRElementsReturnsPKCS10Payload(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{SerialNumber: "7"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), csr)
	writer.PutBytes(tlv.Context(2), make([]byte, 32))
	writer.EndContainer()
	elements, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got, gotNonce, err := nocCSRElements(elements)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificateRequest(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.CheckSignature(); err != nil {
		t.Fatalf("returned CSR signature: %v", err)
	}
	if len(gotNonce) != 32 {
		t.Fatalf("returned CSR nonce length = %d", len(gotNonce))
	}
}

func TestAttestationNonce(t *testing.T) {
	want := []byte("01234567890123456789012345678901")
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUint(tlv.Context(0), 123)
	writer.PutBytes(tlv.Context(2), want)
	writer.EndContainer()
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := attestationNonce(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("nonce = %x, want %x", got, want)
	}
}

func TestDACVendorAndProductFromCertificateSubject(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		Subject: pkix.Name{CommonName: "test DAC", ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: asn1.ObjectIdentifier(oidMatterVID), Value: "FFF1"},
			{Type: asn1.ObjectIdentifier(oidMatterPID), Value: "8000"},
		}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	vendor, product, err := dacVIDPID(certificate)
	if err != nil {
		t.Fatal(err)
	}
	if vendor != 0xFFF1 || product != 0x8000 {
		t.Fatalf("VID/PID = %04X/%04X", vendor, product)
	}
}
