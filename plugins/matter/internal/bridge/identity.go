package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
)

const (
	BridgeVendorID  uint16 = credentials.TestVendorID
	BridgeProductID uint16 = 0x8000
	BridgePort             = 5540
)

var (
	oidMatterVID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 1}
	oidMatterPID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 2}
)

type serverIdentity struct {
	pase *pase.Device
	pai  *x509.Certificate
	dac  *x509.Certificate
	key  *ecdsa.PrivateKey
}

func ensureServerRecord(record ServerRecord) (ServerRecord, *serverIdentity, error) {
	if record.Port == 0 {
		record.Port = BridgePort
	}
	if record.Passcode == 0 || record.Iterations == 0 || len(record.Salt) == 0 {
		parameters, err := commissioning.NewWindowParameters(15 * time.Minute)
		if err != nil {
			return record, nil, err
		}
		record.Passcode, record.Discriminator = parameters.Passcode, parameters.Discriminator
		record.Iterations, record.Salt = parameters.Iterations, parameters.Salt
	}
	identity, err := parseAttestation(record.Attestation)
	if err != nil {
		identity, record.Attestation, err = newAttestation()
		if err != nil {
			return record, nil, err
		}
	}
	parameters := pase.PBKDFParameters{Iterations: record.Iterations, Salt: append([]byte(nil), record.Salt...)}
	identity.pase, err = pase.NewDevice(record.Passcode, parameters)
	if err != nil {
		return record, nil, err
	}
	return record, identity, nil
}

func newAttestation() (*serverIdentity, AttestationRecord, error) {
	paiKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	dacKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	now := time.Now().UTC()
	paiTemplate := &x509.Certificate{
		SerialNumber: randomCertificateSerial(), Subject: pkix.Name{CommonName: "Stulp Matter Bridge Development PAI"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(20, 0, 0), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	paiDER, err := x509.CreateCertificate(rand.Reader, paiTemplate, paiTemplate, &paiKey.PublicKey, paiKey)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	pai, err := x509.ParseCertificate(paiDER)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	dacTemplate := &x509.Certificate{
		SerialNumber: randomCertificateSerial(), Subject: pkix.Name{CommonName: "Stulp Matter Bridge Development DAC",
			ExtraNames: []pkix.AttributeTypeAndValue{{Type: oidMatterVID, Value: "FFF1"}, {Type: oidMatterPID, Value: "8000"}}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(20, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
	}
	dacDER, err := x509.CreateCertificate(rand.Reader, dacTemplate, pai, &dacKey.PublicKey, paiKey)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	dac, err := x509.ParseCertificate(dacDER)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(dacKey)
	if err != nil {
		return nil, AttestationRecord{}, err
	}
	record := AttestationRecord{PAICertificate: paiDER, DACCertificate: dacDER, DACPrivateKey: keyDER}
	return &serverIdentity{pai: pai, dac: dac, key: dacKey}, record, nil
}

func parseAttestation(record AttestationRecord) (*serverIdentity, error) {
	if len(record.PAICertificate) == 0 || len(record.DACCertificate) == 0 || len(record.DACPrivateKey) == 0 {
		return nil, errors.New("Matter bridge attestation is empty")
	}
	pai, err := x509.ParseCertificate(record.PAICertificate)
	if err != nil {
		return nil, err
	}
	dac, err := x509.ParseCertificate(record.DACCertificate)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(record.DACPrivateKey)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(dac.PublicKey) {
		return nil, errors.New("Matter bridge DAC key does not match its certificate")
	}
	if err := dac.CheckSignatureFrom(pai); err != nil {
		return nil, err
	}
	return &serverIdentity{pai: pai, dac: dac, key: key}, nil
}

func randomCertificateSerial() *big.Int {
	value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil || value.Sign() == 0 {
		return big.NewInt(time.Now().UnixNano() & 0x7fffffffffffffff)
	}
	return value
}
