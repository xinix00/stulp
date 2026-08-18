package credentials

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

var (
	oidSignatureECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidPublicKeyECDSA           = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidCurveP256                = asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}
	oidExtKeyUsageServerAuth    = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
	oidExtKeyUsageClientAuth    = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
)

type matterValidity struct {
	NotBefore, NotAfter time.Time
}

type matterPublicKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	PublicKey asn1.BitString
}

type matterTBSCertificate struct {
	Version            int `asn1:"optional,explicit,default:0,tag:0"`
	SerialNumber       *big.Int
	SignatureAlgorithm pkix.AlgorithmIdentifier
	Issuer             asn1.RawValue
	Validity           matterValidity
	Subject            asn1.RawValue
	PublicKey          matterPublicKeyInfo
	UniqueID           asn1.BitString   `asn1:"optional,tag:1"`
	SubjectUniqueID    asn1.BitString   `asn1:"optional,tag:2"`
	Extensions         []pkix.Extension `asn1:"omitempty,optional,explicit,tag:3"`
}

type matterBasicConstraints struct {
	IsCA       bool `asn1:"optional"`
	MaxPathLen int  `asn1:"optional,default:-1"`
}

type matterAuthorityKeyID struct {
	ID []byte `asn1:"optional,tag:0"`
}

type compactCertificate struct {
	serial          []byte
	issuer, subject pkix.RDNSequence
	notBefore       uint32
	notAfter        uint32
	publicKey       []byte
	extensions      []pkix.Extension
	signature       []byte
}

// reconstructMatterTBSCertificate performs the security-sensitive inverse of
// MatterCertificate: it turns the compact Matter TLV fields back into the
// canonical DER TBSCertificate bytes that a Matter peer verifies. Keeping
// this route independent of Go's certificate template encoder makes byte
// drift visible in local tests before a NOC is sent to hardware.
func reconstructMatterTBSCertificate(encoded []byte) ([]byte, error) {
	certificate, err := decodeCompactCertificate(encoded)
	if err != nil {
		return nil, err
	}
	issuer, err := asn1.Marshal(certificate.issuer)
	if err != nil {
		return nil, fmt.Errorf("encode Matter issuer: %w", err)
	}
	subject, err := asn1.Marshal(certificate.subject)
	if err != nil {
		return nil, fmt.Errorf("encode Matter subject: %w", err)
	}
	curveParameters, err := asn1.Marshal(oidCurveP256)
	if err != nil {
		return nil, err
	}
	tbs := matterTBSCertificate{
		Version:      2,
		SerialNumber: new(big.Int).SetBytes(certificate.serial),
		SignatureAlgorithm: pkix.AlgorithmIdentifier{
			Algorithm: oidSignatureECDSAWithSHA256,
		},
		Issuer:   asn1.RawValue{FullBytes: issuer},
		Validity: matterValidity{matterTime(certificate.notBefore), matterTime(certificate.notAfter)},
		Subject:  asn1.RawValue{FullBytes: subject},
		PublicKey: matterPublicKeyInfo{
			Algorithm: pkix.AlgorithmIdentifier{
				Algorithm: oidPublicKeyECDSA, Parameters: asn1.RawValue{FullBytes: curveParameters},
			},
			PublicKey: asn1.BitString{Bytes: certificate.publicKey, BitLength: len(certificate.publicKey) * 8},
		},
		Extensions: certificate.extensions,
	}
	result, err := asn1.Marshal(tbs)
	if err != nil {
		return nil, fmt.Errorf("encode Matter TBSCertificate: %w", err)
	}
	return result, nil
}

// verifyMatterCertificateSignature verifies the raw r||s signature carried by
// a compact Matter certificate over the independently reconstructed TBS.
func verifyMatterCertificateSignature(encoded []byte, issuer *ecdsa.PublicKey) error {
	if issuer == nil {
		return errors.New("Matter certificate verification needs an issuer key")
	}
	certificate, err := decodeCompactCertificate(encoded)
	if err != nil {
		return err
	}
	tbs, err := reconstructMatterTBSCertificate(encoded)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(tbs)
	if !ecdsa.Verify(issuer, hash[:], new(big.Int).SetBytes(certificate.signature[:32]),
		new(big.Int).SetBytes(certificate.signature[32:])) {
		return errors.New("compact Matter certificate signature is invalid after DER reconstruction")
	}
	return nil
}

func decodeCompactCertificate(encoded []byte) (compactCertificate, error) {
	reader := tlv.NewReader(encoded)
	root, err := reader.Next()
	if err != nil || root.Type != tlv.TypeStructure || root.Tag != tlv.Anonymous() {
		return compactCertificate{}, errors.New("compact Matter certificate is not an anonymous structure")
	}
	var certificate compactCertificate
	serial, err := expectCertificateElement(reader, 1, tlv.TypeBytes)
	if err != nil || len(serial.Data) == 0 || len(serial.Data) > 20 || serial.Data[0]&0x80 != 0 {
		return compactCertificate{}, errors.New("compact Matter certificate has an invalid positive serial number")
	}
	certificate.serial = append([]byte(nil), serial.Data...)
	algorithm, err := expectCertificateElement(reader, 2, tlv.TypeUint)
	if err != nil || algorithm.Uint != 1 {
		return compactCertificate{}, errors.New("compact Matter certificate does not use ECDSA-SHA256")
	}
	issuer, err := expectCertificateElement(reader, 3, tlv.TypeList)
	if err != nil {
		return compactCertificate{}, err
	}
	certificate.issuer, err = readMatterDN(reader, issuer)
	if err != nil {
		return compactCertificate{}, fmt.Errorf("compact Matter issuer: %w", err)
	}
	notBefore, err := expectCertificateElement(reader, 4, tlv.TypeUint)
	if err != nil || notBefore.Uint > uint64(^uint32(0)) {
		return compactCertificate{}, errors.New("compact Matter NotBefore is invalid")
	}
	notAfter, err := expectCertificateElement(reader, 5, tlv.TypeUint)
	if err != nil || notAfter.Uint > uint64(^uint32(0)) {
		return compactCertificate{}, errors.New("compact Matter NotAfter is invalid")
	}
	certificate.notBefore, certificate.notAfter = uint32(notBefore.Uint), uint32(notAfter.Uint)
	subject, err := expectCertificateElement(reader, 6, tlv.TypeList)
	if err != nil {
		return compactCertificate{}, err
	}
	certificate.subject, err = readMatterDN(reader, subject)
	if err != nil {
		return compactCertificate{}, fmt.Errorf("compact Matter subject: %w", err)
	}
	publicKeyAlgorithm, err := expectCertificateElement(reader, 7, tlv.TypeUint)
	if err != nil || publicKeyAlgorithm.Uint != 1 {
		return compactCertificate{}, errors.New("compact Matter certificate does not use an EC public key")
	}
	curve, err := expectCertificateElement(reader, 8, tlv.TypeUint)
	if err != nil || curve.Uint != 1 {
		return compactCertificate{}, errors.New("compact Matter certificate does not use P-256")
	}
	publicKey, err := expectCertificateElement(reader, 9, tlv.TypeBytes)
	if err != nil || len(publicKey.Data) != 65 || publicKey.Data[0] != 4 {
		return compactCertificate{}, errors.New("compact Matter certificate has an invalid uncompressed P-256 key")
	}
	certificate.publicKey = append([]byte(nil), publicKey.Data...)
	if _, err := expectCertificateElement(reader, 10, tlv.TypeList); err != nil {
		return compactCertificate{}, err
	}
	certificate.extensions, err = readMatterExtensions(reader)
	if err != nil {
		return compactCertificate{}, err
	}
	signature, err := expectCertificateElement(reader, 11, tlv.TypeBytes)
	if err != nil || len(signature.Data) != 64 {
		return compactCertificate{}, errors.New("compact Matter certificate has no 64-byte ECDSA signature")
	}
	certificate.signature = append([]byte(nil), signature.Data...)
	end, err := reader.Next()
	if err != nil || end.Type != tlv.TypeEnd {
		return compactCertificate{}, errors.New("compact Matter certificate structure is not terminated")
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return compactCertificate{}, errors.New("compact Matter certificate has trailing data")
	}
	return certificate, nil
}

func expectCertificateElement(reader *tlv.Reader, tag uint8, kind tlv.Type) (tlv.Element, error) {
	element, err := reader.Next()
	if err != nil {
		return tlv.Element{}, err
	}
	number, contextTag := element.Tag.ContextNumber()
	if !contextTag || number != tag || element.Type != kind {
		return tlv.Element{}, fmt.Errorf("compact Matter certificate expected tag %d type %d, got %s type %d",
			tag, kind, element.Tag, element.Type)
	}
	return element, nil
}

func readMatterDN(reader *tlv.Reader, _ tlv.Element) (pkix.RDNSequence, error) {
	var sequence pkix.RDNSequence
	for {
		element, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if element.Type == tlv.TypeEnd {
			break
		}
		tag, ok := element.Tag.ContextNumber()
		if !ok || element.Type != tlv.TypeUint {
			return nil, errors.New("Matter DN attribute is not a context-tagged unsigned integer")
		}
		var oid asn1.ObjectIdentifier
		switch tag {
		case 17:
			oid = oidMatterNodeID
		case 20:
			oid = oidMatterRCACID
		case 21:
			oid = oidMatterFabricID
		default:
			return nil, fmt.Errorf("unsupported Matter DN attribute tag %d", tag)
		}
		encoded, err := asn1.MarshalWithParams(fmt.Sprintf("%016X", element.Uint), "utf8")
		if err != nil {
			return nil, err
		}
		sequence = append(sequence, []pkix.AttributeTypeAndValue{{
			Type: oid, Value: asn1.RawValue{FullBytes: encoded},
		}})
	}
	if len(sequence) == 0 {
		return nil, errors.New("Matter DN is empty")
	}
	return sequence, nil
}

func readMatterExtensions(reader *tlv.Reader) ([]pkix.Extension, error) {
	var extensions []pkix.Extension
	for {
		element, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if element.Type == tlv.TypeEnd {
			break
		}
		tag, ok := element.Tag.ContextNumber()
		if !ok {
			return nil, errors.New("Matter certificate extension has no context tag")
		}
		var extension pkix.Extension
		switch tag {
		case 1:
			if element.Type != tlv.TypeStructure {
				return nil, errors.New("Matter BasicConstraints is not a structure")
			}
			value, err := readBasicConstraints(reader)
			if err != nil {
				return nil, err
			}
			extension = pkix.Extension{Id: oidBasicConstraints, Critical: true, Value: value}
		case 2:
			if element.Type != tlv.TypeUint || element.Uint > 0xFFFF {
				return nil, errors.New("Matter KeyUsage is invalid")
			}
			value, err := encodeKeyUsage(uint16(element.Uint))
			if err != nil {
				return nil, err
			}
			extension = pkix.Extension{Id: oidKeyUsage, Critical: true, Value: value}
		case 3:
			if element.Type != tlv.TypeArray {
				return nil, errors.New("Matter ExtendedKeyUsage is not an array")
			}
			value, err := readExtendedKeyUsage(reader)
			if err != nil {
				return nil, err
			}
			extension = pkix.Extension{Id: oidExtendedKeyUsage, Critical: true, Value: value}
		case 4:
			if element.Type != tlv.TypeBytes || len(element.Data) != 20 {
				return nil, errors.New("Matter SubjectKeyIdentifier is not 20 bytes")
			}
			value, err := asn1.Marshal(element.Data)
			if err != nil {
				return nil, err
			}
			extension = pkix.Extension{Id: oidSubjectKeyID, Value: value}
		case 5:
			if element.Type != tlv.TypeBytes || len(element.Data) != 20 {
				return nil, errors.New("Matter AuthorityKeyIdentifier is not 20 bytes")
			}
			value, err := asn1.Marshal(matterAuthorityKeyID{ID: element.Data})
			if err != nil {
				return nil, err
			}
			extension = pkix.Extension{Id: oidAuthorityKeyID, Value: value}
		default:
			return nil, fmt.Errorf("unsupported compact Matter certificate extension tag %d", tag)
		}
		extensions = append(extensions, extension)
	}
	if len(extensions) == 0 {
		return nil, errors.New("compact Matter certificate has no extensions")
	}
	return extensions, nil
}

func readBasicConstraints(reader *tlv.Reader) ([]byte, error) {
	isCA, err := expectCertificateElement(reader, 1, tlv.TypeBool)
	if err != nil {
		return nil, err
	}
	constraints := matterBasicConstraints{IsCA: isCA.Bool, MaxPathLen: -1}
	next, err := reader.Next()
	if err != nil {
		return nil, err
	}
	if next.Type != tlv.TypeEnd {
		tag, ok := next.Tag.ContextNumber()
		if !ok || tag != 2 || next.Type != tlv.TypeUint || next.Uint > uint64(^uint32(0)) {
			return nil, errors.New("Matter BasicConstraints path length is invalid")
		}
		constraints.MaxPathLen = int(next.Uint)
		if end, err := reader.Next(); err != nil || end.Type != tlv.TypeEnd {
			return nil, errors.New("Matter BasicConstraints structure is not terminated")
		}
	}
	return asn1.Marshal(constraints)
}

func readExtendedKeyUsage(reader *tlv.Reader) ([]byte, error) {
	var usages []asn1.ObjectIdentifier
	for {
		element, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if element.Type == tlv.TypeEnd {
			break
		}
		if element.Tag != tlv.Anonymous() || element.Type != tlv.TypeUint {
			return nil, errors.New("Matter ExtendedKeyUsage entry is invalid")
		}
		switch element.Uint {
		case 1:
			usages = append(usages, oidExtKeyUsageServerAuth)
		case 2:
			usages = append(usages, oidExtKeyUsageClientAuth)
		default:
			return nil, fmt.Errorf("unsupported Matter ExtendedKeyUsage %d", element.Uint)
		}
	}
	if len(usages) == 0 {
		return nil, errors.New("Matter ExtendedKeyUsage is empty")
	}
	return asn1.Marshal(usages)
}

func encodeKeyUsage(usage uint16) ([]byte, error) {
	bits := []byte{reverseByte(byte(usage))}
	if high := reverseByte(byte(usage >> 8)); high != 0 {
		bits = append(bits, high)
	}
	return asn1.Marshal(asn1.BitString{Bytes: bits, BitLength: derBitLength(bits)})
}

func reverseByte(value byte) byte {
	value = value>>4 | value<<4
	value = value>>2&0x33 | value<<2&0xCC
	return value>>1&0x55 | value<<1&0xAA
}

func derBitLength(encoded []byte) int {
	length := len(encoded) * 8
	for index := len(encoded) - 1; index >= 0; index-- {
		for bit := byte(1); bit != 0 && encoded[index]&bit == 0; bit <<= 1 {
			length--
		}
		if encoded[index] != 0 {
			break
		}
	}
	return length
}

func matterTime(seconds uint32) time.Time {
	if seconds == 0 {
		return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	return time.Unix(matterEpoch+int64(seconds), 0).UTC()
}
