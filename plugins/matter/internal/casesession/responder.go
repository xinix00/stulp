package casesession

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

// ResponderConfig is the operational identity used by a Matter node while
// accepting CASE. It is primarily useful to native Matter servers and to
// complete controller/device interoperability tests.
type ResponderConfig struct {
	Fabric      *credentials.Fabric
	LocalNodeID uint64
	PrivateKey  *ecdsa.PrivateKey
	NOC         []byte
	ICAC        []byte
}

// Accept authenticates an incoming CASE initiator and installs the resulting
// responder-side secure session in node. Resumption is intentionally not
// implemented; every call accepts a fresh Sigma1/2/3 exchange.
func Accept(ctx context.Context, node *transport.Node, config ResponderConfig) (*transport.SecureSession, error) {
	if node == nil || config.Fabric == nil || config.PrivateKey == nil || config.LocalNodeID == 0 || len(config.NOC) == 0 {
		return nil, errors.New("CASE responder needs a transport and operational identity")
	}
	if err := config.Fabric.Validate(); err != nil {
		return nil, err
	}
	identity, err := parseNOCIdentity(config.NOC)
	if err != nil {
		return nil, fmt.Errorf("CASE responder NOC: %w", err)
	}
	if identity.nodeID != config.LocalNodeID || identity.fabricID != config.Fabric.ID ||
		identity.publicKey.X.Cmp(config.PrivateKey.X) != 0 || identity.publicKey.Y.Cmp(config.PrivateKey.Y) != 0 {
		return nil, errors.New("CASE responder key, NOC and fabric identity do not match")
	}

	exchange, err := node.Accept(ctx)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()
	opcode, sigma1, err := exchange.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if opcode != message.OpcodeCASESigma1 {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			fmt.Errorf("expected CASE Sigma1, got opcode 0x%02x", opcode))
	}
	root, err := decodeRoot(sigma1)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	initiatorRandom, err := bytesField(root, 1, caseRandomSize)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	initiatorSession, err := uintField(root, 2, 0xffff)
	if err != nil || initiatorSession == 0 {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			errors.New("CASE Sigma1 has an invalid initiator session ID"))
	}
	destinationID, err := bytesField(root, 3, sha256.Size)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	expectedDestination, err := config.Fabric.DestinationID(initiatorRandom, config.LocalNodeID)
	if err != nil {
		return nil, err
	}
	if !credentials.EqualBytes(destinationID, expectedDestination) {
		return nil, rejectCASE(exchange, pase.StatusNoSharedTrustRoots,
			errors.New("CASE Sigma1 destination does not select this fabric and node"))
	}
	initiatorPublicBytes, err := bytesField(root, 4, casePublicSize)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	initiatorPublic, err := ecdh.P256().NewPublicKey(initiatorPublicBytes)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			fmt.Errorf("CASE initiator ephemeral key: %w", err))
	}

	responderEphemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := responderEphemeral.ECDH(initiatorPublic)
	if err != nil {
		return nil, err
	}
	responderRandom := make([]byte, caseRandomSize)
	if _, err := rand.Read(responderRandom); err != nil {
		return nil, err
	}
	responderSessionID, err := randomSessionID()
	if err != nil {
		return nil, err
	}
	responderPublic := responderEphemeral.PublicKey().Bytes()
	tbs2, err := encodeTBS(config.NOC, config.ICAC, responderPublic, initiatorPublicBytes)
	if err != nil {
		return nil, err
	}
	signature2, err := signRaw(config.PrivateKey, tbs2)
	if err != nil {
		return nil, err
	}
	resumptionID := make([]byte, resumptionIDSize)
	if _, err := rand.Read(resumptionID); err != nil {
		return nil, err
	}
	tbe2, err := encodeTBE(config.NOC, config.ICAC, signature2, resumptionID)
	if err != nil {
		return nil, err
	}
	operationalIPK, err := config.Fabric.OperationalIPK()
	if err != nil {
		return nil, err
	}
	hashSigma1 := sha256.Sum256(sigma1)
	salt2 := make([]byte, 0, 16+caseRandomSize+casePublicSize+sha256.Size)
	salt2 = append(salt2, operationalIPK...)
	salt2 = append(salt2, responderRandom...)
	salt2 = append(salt2, responderPublic...)
	salt2 = append(salt2, hashSigma1[:]...)
	sigma2Key, err := hkdf.Key(sha256.New, shared, salt2, "Sigma2", 16)
	if err != nil {
		return nil, err
	}
	encrypted2, err := sealCASE(sigma2Key, nonceSigma2, tbe2)
	if err != nil {
		return nil, err
	}
	sigma2, err := encodeSigma2(responderRandom, responderSessionID, responderPublic, encrypted2)
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodeCASESigma2, sigma2); err != nil {
		return nil, fmt.Errorf("send CASE Sigma2: %w", err)
	}

	opcode, sigma3, err := exchange.Receive(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive CASE Sigma3: %w", err)
	}
	if opcode != message.OpcodeCASESigma3 {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			fmt.Errorf("expected CASE Sigma3, got opcode 0x%02x", opcode))
	}
	root3, err := decodeRoot(sigma3)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	encrypted3, err := bytesField(root3, 1, -1)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	hash12 := sha256.New()
	_, _ = hash12.Write(sigma1)
	_, _ = hash12.Write(sigma2)
	salt3 := append(append([]byte(nil), operationalIPK...), hash12.Sum(nil)...)
	sigma3Key, err := hkdf.Key(sha256.New, shared, salt3, "Sigma3", 16)
	if err != nil {
		return nil, err
	}
	decrypted3, err := openCASE(sigma3Key, nonceSigma3, encrypted3)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			fmt.Errorf("authenticate CASE Sigma3: %w", err))
	}
	tbe3, err := parseTBE(decrypted3, false)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	controllerNOC, err := config.Fabric.ControllerMatterCertificate()
	if err != nil {
		return nil, err
	}
	if !credentials.EqualBytes(tbe3.noc, controllerNOC) {
		return nil, rejectCASE(exchange, pase.StatusNoSharedTrustRoots,
			errors.New("CASE initiator presented an unknown operational certificate"))
	}
	controllerIdentity, err := parseNOCIdentity(tbe3.noc)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	if controllerIdentity.nodeID != config.Fabric.ControllerNodeID || controllerIdentity.fabricID != config.Fabric.ID {
		return nil, rejectCASE(exchange, pase.StatusNoSharedTrustRoots,
			errors.New("CASE initiator certificate has the wrong fabric or node ID"))
	}
	tbs3, err := encodeTBS(tbe3.noc, tbe3.icac, initiatorPublicBytes, responderPublic)
	if err != nil {
		return nil, err
	}
	if !verifyRaw(controllerIdentity.publicKey, tbs3, tbe3.signature) {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter,
			errors.New("CASE Sigma3 signature does not match initiator NOC"))
	}
	if err := exchange.Send(ctx, message.OpcodeStatusReport, pase.SessionEstablished().Encode()); err != nil {
		return nil, fmt.Errorf("send CASE result: %w", err)
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
	return node.RegisterSession(transport.SessionConfig{
		LocalID: responderSessionID, PeerID: uint16(initiatorSession),
		LocalNodeID: config.LocalNodeID, PeerNodeID: config.Fabric.ControllerNodeID,
		OutboundKey: keyPack[16:32], InboundKey: keyPack[:16], Remote: exchange.Remote(),
	})
}

func encodeSigma2(randomValue []byte, sessionID uint16, publicKey, encrypted []byte) ([]byte, error) {
	if len(randomValue) != caseRandomSize || sessionID == 0 || len(publicKey) != casePublicSize || len(encrypted) <= 16 {
		return nil, errors.New("invalid CASE Sigma2 inputs")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), randomValue)
	writer.PutUintWidth(tlv.Context(2), uint64(sessionID), 2)
	writer.PutBytes(tlv.Context(3), publicKey)
	writer.PutBytes(tlv.Context(4), encrypted)
	writer.EndContainer()
	return writer.Bytes()
}

func rejectCASE(exchange *transport.Exchange, code uint16, cause error) error {
	_ = exchange.SendOnce(message.OpcodeStatusReport, pase.Failure(code).Encode())
	return cause
}
