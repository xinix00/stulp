package casesession

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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
	Fabric        *credentials.Fabric
	FabricIndex   uint8
	FabricID      uint64
	AdminNodeID   uint64
	IPK           []byte
	RootPublicKey *ecdsa.PublicKey
	LocalNodeID   uint64
	PrivateKey    *ecdsa.PrivateKey
	NOC           []byte
	ICAC          []byte
}

// Accept authenticates an incoming CASE initiator and installs the resulting
// responder-side secure session in node. Resumption is intentionally not
// implemented; every call accepts a fresh Sigma1/2/3 exchange.
func Accept(ctx context.Context, node *transport.Node, config ResponderConfig) (*transport.SecureSession, error) {
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
	return AcceptSigma1(ctx, node, exchange, sigma1, config)
}

// AcceptSigma1 continues CASE after a server's central accept loop has already
// consumed Sigma1. This is the server-side counterpart of PASE ServeRequest.
func AcceptSigma1(ctx context.Context, node *transport.Node, exchange *transport.Exchange, sigma1 []byte, config ResponderConfig) (*transport.SecureSession, error) {
	defer exchange.Close()
	if err := validateResponderConfig(node, config); err != nil {
		return nil, err
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
	expectedDestination, err := responderDestinationID(config, initiatorRandom)
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
	operationalIPK, err := responderOperationalIPK(config)
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
	controllerIdentity, err := parseNOCIdentity(tbe3.noc)
	if err != nil {
		return nil, rejectCASE(exchange, pase.StatusInvalidParameter, err)
	}
	if config.Fabric != nil {
		controllerNOC, certErr := config.Fabric.ControllerMatterCertificate()
		if certErr != nil {
			return nil, certErr
		}
		if !credentials.EqualBytes(tbe3.noc, controllerNOC) {
			return nil, rejectCASE(exchange, pase.StatusNoSharedTrustRoots,
				errors.New("CASE initiator presented an unknown operational certificate"))
		}
	}
	if controllerIdentity.nodeID != responderAdminNodeID(config) || controllerIdentity.fabricID != responderFabricID(config) {
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
		LocalNodeID: config.LocalNodeID, PeerNodeID: controllerIdentity.nodeID, FabricIndex: config.FabricIndex,
		OutboundKey: keyPack[16:32], InboundKey: keyPack[:16], Remote: exchange.Remote(),
	})
}

// AcceptSigma1Any selects the fabric named by Sigma1's destination ID before
// authenticating the controller. Multiple Matter ecosystems commonly reuse
// controller node IDs, so fabric selection must happen before session routing.
func AcceptSigma1Any(ctx context.Context, node *transport.Node, exchange *transport.Exchange, sigma1 []byte,
	configs []ResponderConfig) (*transport.SecureSession, error) {
	root, err := decodeRoot(sigma1)
	if err != nil {
		exchange.Close()
		return nil, err
	}
	initiatorRandom, err := bytesField(root, 1, caseRandomSize)
	if err != nil {
		exchange.Close()
		return nil, err
	}
	destination, err := bytesField(root, 3, sha256.Size)
	if err != nil {
		exchange.Close()
		return nil, err
	}
	for _, config := range configs {
		expected, destinationErr := responderDestinationID(config, initiatorRandom)
		if destinationErr == nil && credentials.EqualBytes(destination, expected) {
			return AcceptSigma1(ctx, node, exchange, sigma1, config)
		}
	}
	defer exchange.Close()
	return nil, rejectCASE(exchange, pase.StatusNoSharedTrustRoots, errors.New("CASE Sigma1 selects no commissioned bridge fabric"))
}

func validateResponderConfig(node *transport.Node, config ResponderConfig) error {
	if node == nil || config.PrivateKey == nil || config.LocalNodeID == 0 || len(config.NOC) == 0 {
		return errors.New("CASE responder needs a transport and operational identity")
	}
	if config.Fabric != nil {
		if err := config.Fabric.Validate(); err != nil {
			return err
		}
	} else if config.FabricIndex == 0 || config.FabricID == 0 || config.AdminNodeID == 0 || len(config.IPK) != 16 || config.RootPublicKey == nil {
		return errors.New("CASE responder external fabric credentials are incomplete")
	}
	identity, err := parseNOCIdentity(config.NOC)
	if err != nil {
		return fmt.Errorf("CASE responder NOC: %w", err)
	}
	if identity.nodeID != config.LocalNodeID || identity.fabricID != responderFabricID(config) ||
		identity.publicKey.X.Cmp(config.PrivateKey.X) != 0 || identity.publicKey.Y.Cmp(config.PrivateKey.Y) != 0 {
		return errors.New("CASE responder key, NOC and fabric identity do not match")
	}
	return nil
}

func responderFabricID(config ResponderConfig) uint64 {
	if config.Fabric != nil {
		return config.Fabric.ID
	}
	return config.FabricID
}

func responderAdminNodeID(config ResponderConfig) uint64 {
	if config.Fabric != nil {
		return config.Fabric.ControllerNodeID
	}
	return config.AdminNodeID
}

func responderRootPublic(config ResponderConfig) *ecdsa.PublicKey {
	if config.Fabric != nil {
		return &config.Fabric.RootKey.PublicKey
	}
	return config.RootPublicKey
}

func responderOperationalIPK(config ResponderConfig) ([]byte, error) {
	if config.Fabric != nil {
		return config.Fabric.OperationalIPK()
	}
	compressed, err := compressedFabricID(responderRootPublic(config), config.FabricID)
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, config.IPK, compressed, "GroupKey v1.0", 16)
}

func responderDestinationID(config ResponderConfig, initiatorRandom []byte) ([]byte, error) {
	if config.Fabric != nil {
		return config.Fabric.DestinationID(initiatorRandom, config.LocalNodeID)
	}
	if len(initiatorRandom) != 32 {
		return nil, errors.New("CASE initiator random must be 32 bytes")
	}
	key, err := responderOperationalIPK(config)
	if err != nil {
		return nil, err
	}
	root := responderRootPublic(config)
	if root == nil {
		return nil, errors.New("CASE fabric has no root public key")
	}
	messageBytes := append([]byte(nil), initiatorRandom...)
	messageBytes = append(messageBytes, elliptic.Marshal(elliptic.P256(), root.X, root.Y)...)
	messageBytes = binary.LittleEndian.AppendUint64(messageBytes, config.FabricID)
	messageBytes = binary.LittleEndian.AppendUint64(messageBytes, config.LocalNodeID)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(messageBytes)
	return mac.Sum(nil), nil
}

func compressedFabricID(root *ecdsa.PublicKey, fabricID uint64) ([]byte, error) {
	if root == nil || root.Curve != elliptic.P256() || fabricID == 0 {
		return nil, errors.New("invalid CASE fabric root")
	}
	public := elliptic.Marshal(elliptic.P256(), root.X, root.Y)
	var salt [8]byte
	binary.BigEndian.PutUint64(salt[:], fabricID)
	return hkdf.Key(sha256.New, public[1:], salt[:], "CompressedFabric", 8)
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
