// Package casesession establishes a certificate-authenticated Matter session
// (CASE) for a controller. It intentionally does not implement resumption yet:
// a fresh Sigma1/2/3 exchange is cheap and avoids persisting another secret.
package casesession

import (
	"context"
	"crypto/aes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"

	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	mattercrypto "github.com/xinix00/stulp/plugins/matter/internal/crypto"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const (
	caseRandomSize   = 32
	casePublicSize   = 65
	caseSignatureLen = 64
	resumptionIDSize = 16
)

var (
	nonceSigma2 = []byte("NCASE_Sigma2N")
	nonceSigma3 = []byte("NCASE_Sigma3N")
)

// Establish authenticates peerNodeID using the exact operational NOC Stulp
// issued during commissioning, then installs the resulting directional keys
// in the transport node.
func Establish(ctx context.Context, node *transport.Node, remote *net.UDPAddr, fabric *credentials.Fabric,
	peerNodeID uint64, expectedPeerNOC []byte) (*transport.SecureSession, error) {
	return EstablishWithRetry(ctx, node, remote, fabric, peerNodeID, expectedPeerNOC, transport.MRPTiming{})
}

// EstablishWithRetry uses the peer's advertised MRP timing: the idle interval
// for the unsecured Sigma1 exchange, because a peer we have not spoken to yet is
// by definition not known to be awake, and the whole timing for the session it
// returns. This matters for sleepy Thread devices: their idle interval can be
// many seconds, while mains-powered nodes generally use the transport default.
func EstablishWithRetry(ctx context.Context, node *transport.Node, remote *net.UDPAddr, fabric *credentials.Fabric,
	peerNodeID uint64, expectedPeerNOC []byte, timing transport.MRPTiming) (*transport.SecureSession, error) {
	if node == nil || remote == nil || fabric == nil {
		return nil, errors.New("CASE needs a transport, remote and fabric")
	}
	if peerNodeID == 0 || len(expectedPeerNOC) == 0 {
		return nil, errors.New("CASE needs the commissioned peer identity")
	}
	if err := fabric.Validate(); err != nil {
		return nil, err
	}

	localSessionID, err := randomSessionID()
	if err != nil {
		return nil, err
	}
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	initiatorRandom := make([]byte, caseRandomSize)
	if _, err := rand.Read(initiatorRandom); err != nil {
		return nil, err
	}
	destinationID, err := fabric.DestinationID(initiatorRandom, peerNodeID)
	if err != nil {
		return nil, err
	}
	sigma1, err := encodeSigma1(initiatorRandom, localSessionID, destinationID, ephemeral.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}

	exchange, err := node.InitiateWithRetry(remote, message.ProtocolSecureChannel, timing.Idle)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()
	if err := exchange.Send(ctx, message.OpcodeCASESigma1, sigma1); err != nil {
		return nil, fmt.Errorf("send CASE Sigma1: %w", err)
	}
	opcode, sigma2, err := exchange.Receive(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive CASE Sigma2: %w", err)
	}
	if opcode == message.OpcodeStatusReport {
		return nil, caseStatusError("Sigma1", sigma2)
	}
	if opcode != message.OpcodeCASESigma2 {
		return nil, fmt.Errorf("expected CASE Sigma2, got opcode 0x%02x", opcode)
	}

	parsed, err := parseSigma2(sigma2)
	if err != nil {
		return nil, err
	}
	responderPublic, err := ecdh.P256().NewPublicKey(parsed.responderPublic)
	if err != nil {
		return nil, fmt.Errorf("CASE responder ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(responderPublic)
	if err != nil {
		return nil, err
	}
	operationalIPK, err := fabric.OperationalIPK()
	if err != nil {
		return nil, err
	}
	hashSigma1 := sha256.Sum256(sigma1)
	salt2 := make([]byte, 0, 16+caseRandomSize+casePublicSize+sha256.Size)
	salt2 = append(salt2, operationalIPK...)
	salt2 = append(salt2, parsed.responderRandom...)
	salt2 = append(salt2, parsed.responderPublic...)
	salt2 = append(salt2, hashSigma1[:]...)
	sigma2Key, err := hkdf.Key(sha256.New, shared, salt2, "Sigma2", 16)
	if err != nil {
		return nil, err
	}
	decrypted2, err := openCASE(sigma2Key, nonceSigma2, parsed.encrypted)
	if err != nil {
		return nil, fmt.Errorf("authenticate CASE Sigma2: %w", err)
	}
	tbe2, err := parseTBE(decrypted2, true)
	if err != nil {
		return nil, fmt.Errorf("decode CASE Sigma2 encrypted data: %w", err)
	}
	if !credentials.EqualBytes(tbe2.noc, expectedPeerNOC) {
		return nil, errors.New("CASE peer presented a different operational certificate")
	}
	peerIdentity, err := parseNOCIdentity(tbe2.noc)
	if err != nil {
		return nil, err
	}
	if peerIdentity.nodeID != peerNodeID || peerIdentity.fabricID != fabric.ID {
		return nil, errors.New("CASE peer certificate has the wrong fabric or node ID")
	}
	tbs2, err := encodeTBS(tbe2.noc, tbe2.icac, parsed.responderPublic, ephemeral.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	if !verifyRaw(peerIdentity.publicKey, tbs2, tbe2.signature) {
		return nil, errors.New("CASE Sigma2 signature does not match peer NOC")
	}

	controllerNOC, err := fabric.ControllerMatterCertificate()
	if err != nil {
		return nil, err
	}
	tbs3, err := encodeTBS(controllerNOC, nil, ephemeral.PublicKey().Bytes(), parsed.responderPublic)
	if err != nil {
		return nil, err
	}
	signature3, err := signRaw(fabric.ControllerKey, tbs3)
	if err != nil {
		return nil, err
	}
	tbe3, err := encodeTBE(controllerNOC, nil, signature3, nil)
	if err != nil {
		return nil, err
	}
	hash12 := sha256.New()
	_, _ = hash12.Write(sigma1)
	_, _ = hash12.Write(sigma2)
	salt3 := append(append([]byte(nil), operationalIPK...), hash12.Sum(nil)...)
	sigma3Key, err := hkdf.Key(sha256.New, shared, salt3, "Sigma3", 16)
	if err != nil {
		return nil, err
	}
	encrypted3, err := sealCASE(sigma3Key, nonceSigma3, tbe3)
	if err != nil {
		return nil, err
	}
	sigma3, err := encodeSigma3(encrypted3)
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodeCASESigma3, sigma3); err != nil {
		return nil, fmt.Errorf("send CASE Sigma3: %w", err)
	}
	opcode, statusBytes, err := exchange.Receive(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive CASE result: %w", err)
	}
	_ = exchange.Acknowledge()
	if opcode != message.OpcodeStatusReport {
		return nil, fmt.Errorf("expected CASE status report, got opcode 0x%02x", opcode)
	}
	status, err := pase.DecodeStatusReport(statusBytes)
	if err != nil {
		return nil, err
	}
	if !status.OK() {
		return nil, fmt.Errorf("peer rejected CASE: %w", status)
	}

	hash123 := sha256.New()
	_, _ = hash123.Write(sigma1)
	_, _ = hash123.Write(sigma2)
	_, _ = hash123.Write(sigma3)
	sessionSalt := append(append([]byte(nil), operationalIPK...), hash123.Sum(nil)...)
	keyPack, err := hkdf.Key(sha256.New, shared, sessionSalt, "SessionKeys", 48)
	if err != nil {
		return nil, err
	}
	// The timing that got Sigma1 through belongs to the session too: the peer
	// that needed seventeen seconds to answer a handshake needs them for a read
	// as well. Without this the session falls back to the transport default and
	// every exchange after CASE is impatient again.
	return node.RegisterSession(transport.SessionConfig{
		LocalID: localSessionID, PeerID: parsed.responderSessionID,
		LocalNodeID: fabric.ControllerNodeID, PeerNodeID: peerNodeID,
		OutboundKey: keyPack[:16], InboundKey: keyPack[16:32], Remote: remote,
		Timing: timing,
	})
}

func encodeSigma1(randomValue []byte, sessionID uint16, destinationID, publicKey []byte) ([]byte, error) {
	if len(randomValue) != caseRandomSize || sessionID == 0 || len(destinationID) != sha256.Size || len(publicKey) != casePublicSize {
		return nil, errors.New("invalid CASE Sigma1 inputs")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), randomValue)
	writer.PutUintWidth(tlv.Context(2), uint64(sessionID), 2)
	writer.PutBytes(tlv.Context(3), destinationID)
	writer.PutBytes(tlv.Context(4), publicKey)
	writer.StartStructure(tlv.Context(5))
	writer.PutUintWidth(tlv.Context(1), 5000, 4)
	writer.PutUintWidth(tlv.Context(2), 300, 4)
	writer.PutUintWidth(tlv.Context(3), 4000, 2)
	writer.EndContainer()
	writer.EndContainer()
	return writer.Bytes()
}

type sigma2Message struct {
	responderRandom    []byte
	responderSessionID uint16
	responderPublic    []byte
	encrypted          []byte
}

func parseSigma2(encoded []byte) (sigma2Message, error) {
	root, err := decodeRoot(encoded)
	if err != nil {
		return sigma2Message{}, err
	}
	randomValue, err := bytesField(root, 1, caseRandomSize)
	if err != nil {
		return sigma2Message{}, err
	}
	sessionID, err := uintField(root, 2, 0xFFFF)
	if err != nil || sessionID == 0 {
		return sigma2Message{}, errors.New("CASE Sigma2 has an invalid responder session ID")
	}
	publicKey, err := bytesField(root, 3, casePublicSize)
	if err != nil {
		return sigma2Message{}, err
	}
	encrypted, err := bytesField(root, 4, -1)
	if err != nil || len(encrypted) <= mattercrypto.TagSize {
		return sigma2Message{}, errors.New("CASE Sigma2 encrypted data is missing")
	}
	return sigma2Message{randomValue, uint16(sessionID), publicKey, encrypted}, nil
}

func encodeSigma3(encrypted []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), encrypted)
	writer.EndContainer()
	return writer.Bytes()
}

type tbeData struct {
	noc, icac, signature, resumptionID []byte
}

func parseTBE(encoded []byte, requireResumption bool) (tbeData, error) {
	root, err := decodeRoot(encoded)
	if err != nil {
		return tbeData{}, err
	}
	noc, err := bytesField(root, 1, -1)
	if err != nil || len(noc) > 400 {
		return tbeData{}, errors.New("CASE TBE has no valid sender NOC")
	}
	var icac []byte
	if value, ok := root.field(2); ok {
		if value.kind != tlv.TypeBytes || len(value.data) > 400 {
			return tbeData{}, errors.New("CASE TBE has an invalid ICAC")
		}
		icac = value.data
	}
	signature, err := bytesField(root, 3, caseSignatureLen)
	if err != nil {
		return tbeData{}, err
	}
	var resumption []byte
	if requireResumption {
		resumption, err = bytesField(root, 4, resumptionIDSize)
		if err != nil {
			return tbeData{}, err
		}
	}
	return tbeData{noc: noc, icac: icac, signature: signature, resumptionID: resumption}, nil
}

func encodeTBE(noc, icac, signature, resumptionID []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), noc)
	if len(icac) > 0 {
		writer.PutBytes(tlv.Context(2), icac)
	}
	writer.PutBytes(tlv.Context(3), signature)
	if len(resumptionID) > 0 {
		writer.PutBytes(tlv.Context(4), resumptionID)
	}
	writer.EndContainer()
	return writer.Bytes()
}

func encodeTBS(noc, icac, senderPublic, receiverPublic []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), noc)
	if len(icac) > 0 {
		writer.PutBytes(tlv.Context(2), icac)
	}
	writer.PutBytes(tlv.Context(3), senderPublic)
	writer.PutBytes(tlv.Context(4), receiverPublic)
	writer.EndContainer()
	return writer.Bytes()
}

func sealCASE(key, nonce, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := mattercrypto.NewCCM(block, mattercrypto.TagSize, len(nonce))
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, nil), nil
}

func openCASE(key, nonce, encrypted []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := mattercrypto.NewCCM(block, mattercrypto.TagSize, len(nonce))
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, encrypted, nil)
}

func signRaw(key *ecdsa.PrivateKey, messageBytes []byte) ([]byte, error) {
	hash := sha256.Sum256(messageBytes)
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return nil, err
	}
	result := make([]byte, caseSignatureLen)
	r.FillBytes(result[:32])
	s.FillBytes(result[32:])
	return result, nil
}

func verifyRaw(key *ecdsa.PublicKey, messageBytes, signature []byte) bool {
	if key == nil || len(signature) != caseSignatureLen {
		return false
	}
	hash := sha256.Sum256(messageBytes)
	return ecdsa.Verify(key, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

type nocIdentity struct {
	publicKey        *ecdsa.PublicKey
	nodeID, fabricID uint64
}

func parseNOCIdentity(encoded []byte) (nocIdentity, error) {
	root, err := decodeRoot(encoded)
	if err != nil {
		return nocIdentity{}, fmt.Errorf("decode peer NOC: %w", err)
	}
	publicBytes, err := bytesField(root, 9, casePublicSize)
	if err != nil {
		return nocIdentity{}, err
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), publicBytes)
	if x == nil {
		return nocIdentity{}, errors.New("peer NOC contains an invalid P-256 key")
	}
	subject, ok := root.field(6)
	if !ok || subject.kind != tlv.TypeList {
		return nocIdentity{}, errors.New("peer NOC has no subject")
	}
	nodeID, err := uintField(subject, 17, ^uint64(0))
	if err != nil {
		return nocIdentity{}, err
	}
	fabricID, err := uintField(subject, 21, ^uint64(0))
	if err != nil {
		return nocIdentity{}, err
	}
	return nocIdentity{publicKey: &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nodeID: nodeID, fabricID: fabricID}, nil
}

type value struct {
	tag      tlv.Tag
	kind     tlv.Type
	unsigned uint64
	data     []byte
	children []value
}

func (v value) field(number uint8) (value, bool) {
	for _, child := range v.children {
		if tag, ok := child.tag.ContextNumber(); ok && tag == number {
			return child, true
		}
	}
	return value{}, false
}

func decodeRoot(encoded []byte) (value, error) {
	reader := tlv.NewReader(encoded)
	root, err := readValue(reader)
	if err != nil {
		return value{}, err
	}
	if root.kind != tlv.TypeStructure {
		return value{}, errors.New("CASE payload root is not a structure")
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return value{}, errors.New("CASE payload has trailing TLV")
	}
	return root, nil
}

func readValue(reader *tlv.Reader) (value, error) {
	element, err := reader.Next()
	if err != nil {
		return value{}, err
	}
	return readElement(reader, element)
}

func readElement(reader *tlv.Reader, element tlv.Element) (value, error) {
	result := value{tag: element.Tag, kind: element.Type, unsigned: element.Uint, data: append([]byte(nil), element.Data...)}
	if element.Type != tlv.TypeStructure && element.Type != tlv.TypeArray && element.Type != tlv.TypeList {
		return result, nil
	}
	for {
		child, err := reader.Next()
		if err != nil {
			return value{}, err
		}
		if child.Type == tlv.TypeEnd {
			return result, nil
		}
		decoded, err := readElement(reader, child)
		if err != nil {
			return value{}, err
		}
		result.children = append(result.children, decoded)
	}
}

func bytesField(parent value, number uint8, length int) ([]byte, error) {
	field, ok := parent.field(number)
	if !ok || field.kind != tlv.TypeBytes || (length >= 0 && len(field.data) != length) {
		return nil, fmt.Errorf("CASE field %d is missing or has an invalid byte-string length", number)
	}
	return append([]byte(nil), field.data...), nil
}

func uintField(parent value, number uint8, maximum uint64) (uint64, error) {
	field, ok := parent.field(number)
	if !ok || field.kind != tlv.TypeUint || field.unsigned > maximum {
		return 0, fmt.Errorf("CASE field %d is missing or invalid", number)
	}
	return field.unsigned, nil
}

func randomSessionID() (uint16, error) {
	for {
		var encoded [2]byte
		if _, err := rand.Read(encoded[:]); err != nil {
			return 0, err
		}
		if value := binary.LittleEndian.Uint16(encoded[:]); value != 0 {
			return value, nil
		}
	}
}

func caseStatusError(stage string, encoded []byte) error {
	status, err := pase.DecodeStatusReport(encoded)
	if err != nil {
		return fmt.Errorf("peer rejected CASE %s with unreadable status: %w", stage, err)
	}
	return fmt.Errorf("peer rejected CASE %s: %w", stage, status)
}
