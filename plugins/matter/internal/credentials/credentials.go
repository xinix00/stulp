// Package credentials owns a Matter fabric's operational identity. It
// creates standards-shaped X.509 certificates, converts them to Matter's
// compact TLV certificate representation and derives fabric-scoped keys.
package credentials

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // Matter operational certificate key identifiers mandate SHA-1.
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

const (
	TestVendorID uint16 = 0xFFF1
	matterEpoch         = 946684800 // 2000-01-01T00:00:00Z, Unix seconds.
)

var (
	oidMatterNodeID   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 1, 1}
	oidMatterRCACID   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 1, 4}
	oidMatterFabricID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 1, 5}

	oidBasicConstraints = asn1.ObjectIdentifier{2, 5, 29, 19}
	oidKeyUsage         = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidExtendedKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidSubjectKeyID     = asn1.ObjectIdentifier{2, 5, 29, 14}
	oidAuthorityKeyID   = asn1.ObjectIdentifier{2, 5, 29, 35}
)

// Fabric is the complete long-lived controller identity. The private keys
// must be persisted together: losing the root means existing Matter nodes
// have to be commissioned onto a new fabric.
type Fabric struct {
	ID               uint64
	RootID           uint64
	ControllerNodeID uint64
	IPK              []byte
	RootKey          *ecdsa.PrivateKey
	RootCertificate  *x509.Certificate
	ControllerKey    *ecdsa.PrivateKey
	ControllerNOC    *x509.Certificate
}

func NewFabric(fabricID, rootID, controllerNodeID uint64, now time.Time) (*Fabric, error) {
	if fabricID == 0 || rootID == 0 || controllerNodeID == 0 {
		return nil, errors.New("fabric, root and controller node IDs must be non-zero")
	}
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	rootCert, err := createRoot(rootKey, rootID, now)
	if err != nil {
		return nil, err
	}
	controllerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	controllerCert, err := signNode(rootCert, rootKey, &controllerKey.PublicKey, fabricID, controllerNodeID, now)
	if err != nil {
		return nil, err
	}
	ipk := make([]byte, 16)
	if _, err := rand.Read(ipk); err != nil {
		return nil, err
	}
	return &Fabric{
		ID: fabricID, RootID: rootID, ControllerNodeID: controllerNodeID,
		IPK: ipk, RootKey: rootKey, RootCertificate: rootCert,
		ControllerKey: controllerKey, ControllerNOC: controllerCert,
	}, nil
}

func (f *Fabric) Validate() error {
	if f == nil || f.ID == 0 || f.RootID == 0 || f.ControllerNodeID == 0 {
		return errors.New("incomplete Matter fabric identity")
	}
	if len(f.IPK) != 16 || f.RootKey == nil || f.RootCertificate == nil || f.ControllerKey == nil || f.ControllerNOC == nil {
		return errors.New("Matter fabric credentials are incomplete")
	}
	if !f.RootKey.PublicKey.Equal(f.RootCertificate.PublicKey) {
		return errors.New("root certificate does not match its private key")
	}
	if !f.ControllerKey.PublicKey.Equal(f.ControllerNOC.PublicKey) {
		return errors.New("controller NOC does not match its private key")
	}
	if err := f.ControllerNOC.CheckSignatureFrom(f.RootCertificate); err != nil {
		return fmt.Errorf("controller NOC is not signed by fabric root: %w", err)
	}
	return nil
}

func (f *Fabric) SignNode(publicKey *ecdsa.PublicKey, nodeID uint64, now time.Time) (*x509.Certificate, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return signNode(f.RootCertificate, f.RootKey, publicKey, f.ID, nodeID, now)
}

func (f *Fabric) RootMatterCertificate() ([]byte, error) {
	return MatterCertificate(f.RootCertificate)
}

func (f *Fabric) ControllerMatterCertificate() ([]byte, error) {
	return MatterCertificate(f.ControllerNOC)
}

// CompressedID is the eight-byte compressed fabric ID used in operational
// discovery instance names.
func (f *Fabric) CompressedID() ([8]byte, error) {
	var result [8]byte
	if err := f.Validate(); err != nil {
		return result, err
	}
	publicKey := elliptic.Marshal(elliptic.P256(), f.RootKey.X, f.RootKey.Y)
	var salt [8]byte
	binary.BigEndian.PutUint64(salt[:], f.ID)
	derived, err := hkdf.Key(sha256.New, publicKey[1:], salt[:], "CompressedFabric", len(result))
	if err != nil {
		return result, err
	}
	copy(result[:], derived)
	return result, nil
}

func (f *Fabric) OperationalIPK() ([]byte, error) {
	compressed, err := f.CompressedID()
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, f.IPK, compressed[:], "GroupKey v1.0", 16)
}

func (f *Fabric) DestinationID(initiatorRandom []byte, peerNodeID uint64) ([]byte, error) {
	if len(initiatorRandom) != 32 {
		return nil, fmt.Errorf("CASE initiator random must be 32 bytes, got %d", len(initiatorRandom))
	}
	key, err := f.OperationalIPK()
	if err != nil {
		return nil, err
	}
	rootPublic := elliptic.Marshal(elliptic.P256(), f.RootKey.X, f.RootKey.Y)
	message := make([]byte, 0, 32+65+16)
	message = append(message, initiatorRandom...)
	message = append(message, rootPublic...)
	message = binary.LittleEndian.AppendUint64(message, f.ID)
	message = binary.LittleEndian.AppendUint64(message, peerNodeID)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return mac.Sum(nil), nil
}

func createRoot(key *ecdsa.PrivateKey, rootID uint64, now time.Time) (*x509.Certificate, error) {
	name, err := matterName(oidMatterRCACID, rootID)
	if err != nil {
		return nil, err
	}
	skid := keyID(&key.PublicKey)
	template := &x509.Certificate{
		SerialNumber:       randomSerial(),
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		Subject:            name,
		NotBefore:          cleanTime(now.Add(-5 * time.Minute)),
		NotAfter:           cleanTime(now.AddDate(20, 0, 0)),
		IsCA:               true,
		ExtraExtensions: []pkix.Extension{
			{Id: oidBasicConstraints, Critical: true, Value: []byte{0x30, 0x03, 0x01, 0x01, 0xff}},
			{Id: oidKeyUsage, Critical: true, Value: []byte{0x03, 0x02, 0x01, 0x06}},
			{Id: oidSubjectKeyID, Value: append([]byte{0x04, 0x14}, skid...)},
			{Id: oidAuthorityKeyID, Value: append([]byte{0x30, 0x16, 0x80, 0x14}, skid...)},
		},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(encoded)
}

func signNode(root *x509.Certificate, rootKey *ecdsa.PrivateKey, publicKey *ecdsa.PublicKey,
	fabricID, nodeID uint64, now time.Time) (*x509.Certificate, error) {
	if publicKey == nil || publicKey.Curve != elliptic.P256() {
		return nil, errors.New("Matter operational keys must use P-256")
	}
	name, err := nodeName(nodeID, fabricID)
	if err != nil {
		return nil, err
	}
	skid := keyID(publicKey)
	// EKU order is ClientAuth, ServerAuth. The compact certificate preserves
	// this exact order so converting it back produces the signed DER TBS.
	extendedUsage := []byte{0x30, 0x14, 0x06, 0x08, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x03, 0x02,
		0x06, 0x08, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x03, 0x01}
	template := &x509.Certificate{
		SerialNumber:       randomSerial(),
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		Subject:            name,
		NotBefore:          cleanTime(now.Add(-5 * time.Minute)),
		NotAfter:           cleanTime(now.AddDate(20, 0, 0)),
		ExtraExtensions: []pkix.Extension{
			{Id: oidBasicConstraints, Critical: true, Value: []byte{0x30, 0x00}},
			{Id: oidKeyUsage, Critical: true, Value: []byte{0x03, 0x02, 0x07, 0x80}},
			{Id: oidExtendedKeyUsage, Critical: true, Value: extendedUsage},
			{Id: oidSubjectKeyID, Value: append([]byte{0x04, 0x14}, skid...)},
			{Id: oidAuthorityKeyID, Value: append([]byte{0x30, 0x16, 0x80, 0x14}, root.SubjectKeyId...)},
		},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, root, publicKey, rootKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(encoded)
}

func matterName(oid asn1.ObjectIdentifier, value uint64) (pkix.Name, error) {
	encoded, err := asn1.MarshalWithParams(fmt.Sprintf("%016X", value), "utf8")
	if err != nil {
		return pkix.Name{}, err
	}
	return pkix.Name{ExtraNames: []pkix.AttributeTypeAndValue{{Type: oid, Value: asn1.RawValue{FullBytes: encoded}}}}, nil
}

func nodeName(nodeID, fabricID uint64) (pkix.Name, error) {
	nodeValue, err := asn1.MarshalWithParams(fmt.Sprintf("%016X", nodeID), "utf8")
	if err != nil {
		return pkix.Name{}, err
	}
	fabricValue, err := asn1.MarshalWithParams(fmt.Sprintf("%016X", fabricID), "utf8")
	if err != nil {
		return pkix.Name{}, err
	}
	return pkix.Name{ExtraNames: []pkix.AttributeTypeAndValue{
		{Type: oidMatterNodeID, Value: asn1.RawValue{FullBytes: nodeValue}},
		{Type: oidMatterFabricID, Value: asn1.RawValue{FullBytes: fabricValue}},
	}}, nil
}

func keyID(publicKey *ecdsa.PublicKey) []byte {
	hash := sha1.Sum(elliptic.Marshal(elliptic.P256(), publicKey.X, publicKey.Y))
	return append([]byte(nil), hash[:]...)
}

func randomSerial() *big.Int {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	value[0] &= 0x7F
	value[0] |= 1
	return new(big.Int).SetBytes(value)
}

func cleanTime(value time.Time) time.Time { return value.UTC().Truncate(time.Second) }

// MatterCertificate converts an operational X.509 certificate to the compact
// Matter TLV certificate. The ECDSA signature remains the signature over the
// canonical DER TBSCertificate; Matter peers reconstruct that DER form when
// validating it.
func MatterCertificate(certificate *x509.Certificate) ([]byte, error) {
	if certificate == nil || certificate.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		return nil, errors.New("Matter certificate requires ECDSA-with-SHA256")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return nil, errors.New("Matter certificate public key is not P-256")
	}
	issuer, err := matterDN(certificate.Issuer)
	if err != nil {
		return nil, fmt.Errorf("certificate issuer: %w", err)
	}
	subject, err := matterDN(certificate.Subject)
	if err != nil {
		return nil, fmt.Errorf("certificate subject: %w", err)
	}
	notBefore, err := chipTime(certificate.NotBefore)
	if err != nil {
		return nil, err
	}
	notAfter, err := chipTime(certificate.NotAfter)
	if err != nil {
		return nil, err
	}
	signature, err := rawSignature(certificate.Signature)
	if err != nil {
		return nil, err
	}

	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), certificate.SerialNumber.Bytes())
	writer.PutUintWidth(tlv.Context(2), 1, 1)
	writeDN(&writer, tlv.Context(3), issuer)
	writer.PutUintWidth(tlv.Context(4), uint64(notBefore), 4)
	writer.PutUintWidth(tlv.Context(5), uint64(notAfter), 4)
	writeDN(&writer, tlv.Context(6), subject)
	writer.PutUintWidth(tlv.Context(7), 1, 1)
	writer.PutUintWidth(tlv.Context(8), 1, 1)
	writer.PutBytes(tlv.Context(9), elliptic.Marshal(elliptic.P256(), publicKey.X, publicKey.Y))
	writer.StartList(tlv.Context(10))
	writer.StartStructure(tlv.Context(1))
	writer.PutBool(tlv.Context(1), certificate.IsCA)
	writer.EndContainer()
	writer.PutUintWidth(tlv.Context(2), uint64(certificate.KeyUsage), 2)
	if len(certificate.ExtKeyUsage) > 0 {
		writer.StartArray(tlv.Context(3))
		for _, usage := range certificate.ExtKeyUsage {
			switch usage {
			case x509.ExtKeyUsageServerAuth:
				writer.PutUintWidth(tlv.Anonymous(), 1, 1)
			case x509.ExtKeyUsageClientAuth:
				writer.PutUintWidth(tlv.Anonymous(), 2, 1)
			default:
				return nil, fmt.Errorf("unsupported Matter extended key usage %d", usage)
			}
		}
		writer.EndContainer()
	}
	if len(certificate.SubjectKeyId) != 20 || len(certificate.AuthorityKeyId) != 20 {
		return nil, errors.New("Matter certificate needs 20-byte subject and authority key IDs")
	}
	writer.PutBytes(tlv.Context(4), certificate.SubjectKeyId)
	writer.PutBytes(tlv.Context(5), certificate.AuthorityKeyId)
	writer.EndContainer()
	writer.PutBytes(tlv.Context(11), signature)
	writer.EndContainer()
	return writer.Bytes()
}

type distinguishedName struct {
	nodeID, rootID, fabricID *uint64
}

func matterDN(name pkix.Name) (distinguishedName, error) {
	var result distinguishedName
	for _, attribute := range name.Names {
		var target **uint64
		switch {
		case attribute.Type.Equal(oidMatterNodeID):
			target = &result.nodeID
		case attribute.Type.Equal(oidMatterRCACID):
			target = &result.rootID
		case attribute.Type.Equal(oidMatterFabricID):
			target = &result.fabricID
		default:
			continue
		}
		text, ok := attribute.Value.(string)
		if !ok {
			return result, errors.New("Matter DN value is not a string")
		}
		value := new(big.Int)
		if _, ok := value.SetString(text, 16); !ok || !value.IsUint64() {
			return result, fmt.Errorf("Matter DN value %q is not 64-bit hexadecimal", text)
		}
		converted := value.Uint64()
		*target = &converted
	}
	if result.rootID == nil && result.nodeID == nil {
		return result, errors.New("Matter DN has neither RCAC nor node ID")
	}
	return result, nil
}

func writeDN(writer *tlv.Writer, tag tlv.Tag, name distinguishedName) {
	writer.StartList(tag)
	if name.nodeID != nil {
		writer.PutUintWidth(tlv.Context(17), *name.nodeID, 8)
	}
	if name.rootID != nil {
		writer.PutUintWidth(tlv.Context(20), *name.rootID, 8)
	}
	if name.fabricID != nil {
		writer.PutUintWidth(tlv.Context(21), *name.fabricID, 8)
	}
	writer.EndContainer()
}

func chipTime(value time.Time) (uint32, error) {
	seconds := value.Unix() - matterEpoch
	if seconds < 0 || seconds > int64(^uint32(0)) {
		return 0, fmt.Errorf("certificate time %s is outside Matter epoch", value)
	}
	return uint32(seconds), nil
}

type ecdsaSignature struct{ R, S *big.Int }

func rawSignature(der []byte) ([]byte, error) {
	var signature ecdsaSignature
	rest, err := asn1.Unmarshal(der, &signature)
	if err != nil || len(rest) != 0 || signature.R == nil || signature.S == nil {
		return nil, errors.New("invalid ECDSA certificate signature")
	}
	result := make([]byte, 64)
	signature.R.FillBytes(result[:32])
	signature.S.FillBytes(result[32:])
	return result, nil
}

func MarshalPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(key)
}

func ParsePrivateKey(encoded []byte) (*ecdsa.PrivateKey, error) {
	key, err := x509.ParsePKCS8PrivateKey(encoded)
	if err != nil {
		return nil, err
	}
	result, ok := key.(*ecdsa.PrivateKey)
	if !ok || result.Curve != elliptic.P256() {
		return nil, errors.New("stored Matter key is not P-256 ECDSA")
	}
	return result, nil
}

// EqualBytes compares credential-bearing byte strings in constant time.
func EqualBytes(left, right []byte) bool { return hmac.Equal(left, right) }
