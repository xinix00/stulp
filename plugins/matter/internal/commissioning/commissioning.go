// Package commissioning implements the small cluster-specific portion of a
// Matter commissioning flow. The transport and Interaction Model stay
// generic; this package only knows the General Commissioning and Operational
// Credentials command schemas.
package commissioning

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

const (
	ClusterGeneralCommissioning     uint32 = 0x0030
	ClusterOperationalCredentials   uint32 = 0x003E
	CommandArmFailSafe              uint32 = 0x00
	CommandArmFailSafeResponse      uint32 = 0x01
	CommandSetRegulatoryConfig      uint32 = 0x02
	CommandSetRegulatoryConfigRsp   uint32 = 0x03
	CommandCommissioningComplete    uint32 = 0x04
	CommandCommissioningCompleteRsp uint32 = 0x05
	CommandCSRRequest               uint32 = 0x04
	CommandCSRResponse              uint32 = 0x05
	CommandAddNOC                   uint32 = 0x06
	CommandNOCResponse              uint32 = 0x08
	CommandRemoveFabric             uint32 = 0x0A
	CommandAddTrustedRoot           uint32 = 0x0B
	CommandAttestationRequest       uint32 = 0x00
	CommandAttestationResponse      uint32 = 0x01
	CommandCertificateChainRequest  uint32 = 0x02
	CommandCertificateChainResponse uint32 = 0x03
	CertificateTypeDAC              uint8  = 1
	CertificateTypePAI              uint8  = 2
)

const (
	RegulatoryIndoor            uint8 = 0
	RegulatoryOutdoor           uint8 = 1
	RegulatoryIndoorOutdoor     uint8 = 2
	AttributeRegulatoryConfig         = 0x0002
	AttributeLocationCapability       = 0x0003
)

// Client performs commissioning commands over an already established PASE
// or CASE session.
type Client struct {
	IM       im.Client
	Endpoint uint16
}

type CommissioningError struct {
	Status uint8
	Debug  string
}

type Attestation struct {
	DAC       *x509.Certificate
	PAI       *x509.Certificate
	VendorID  uint16
	ProductID uint16
}

// Attest verifies that the device holds the DAC private key and that its DAC
// chains to the supplied PAI. A PAA/DCL trust-store check is deliberately a
// separate policy layer; this method does not pretend that PAI possession is
// equivalent to certification by CSA.
func (c Client) Attest(ctx context.Context, attestationChallenge []byte, expectedVendorID, expectedProductID uint16) (Attestation, error) {
	if len(attestationChallenge) == 0 {
		return Attestation{}, errors.New("PASE supplied no attestation challenge")
	}
	// Follow the commissioning order used by the Matter reference controller.
	// Some accessories are stricter than others about PAI -> DAC -> attestation.
	paiBytes, err := c.certificateChain(ctx, CertificateTypePAI)
	if err != nil {
		return Attestation{}, fmt.Errorf("request Matter PAI: %w", err)
	}
	dacBytes, err := c.certificateChain(ctx, CertificateTypeDAC)
	if err != nil {
		return Attestation{}, fmt.Errorf("request Matter DAC: %w", err)
	}
	pai, err := x509.ParseCertificate(paiBytes)
	if err != nil {
		return Attestation{}, fmt.Errorf("parse Matter PAI: %w", err)
	}
	dac, err := x509.ParseCertificate(dacBytes)
	if err != nil {
		return Attestation{}, fmt.Errorf("parse Matter DAC: %w", err)
	}
	if err := dac.CheckSignatureFrom(pai); err != nil {
		return Attestation{}, fmt.Errorf("Matter DAC is not signed by PAI: %w", err)
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return Attestation{}, err
	}
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandAttestationRequest, CommandAttestationResponse,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), nonce)
			writer.EndContainer()
		})
	if err != nil {
		return Attestation{}, err
	}
	if result.Fields == nil {
		return Attestation{}, errors.New("AttestationResponse has no fields")
	}
	elements, ok := result.Fields.Field(0)
	if !ok || elements.Type != tlv.TypeBytes {
		return Attestation{}, errors.New("AttestationResponse has no attestation elements")
	}
	signature, ok := result.Fields.Field(1)
	if !ok || signature.Type != tlv.TypeBytes || len(signature.Data) != 64 {
		return Attestation{}, errors.New("AttestationResponse has no 64-byte signature")
	}
	returnedNonce, err := attestationNonce(elements.Data)
	if err != nil || !equalBytes(returnedNonce, nonce) {
		return Attestation{}, errors.New("attestation elements do not echo Stulp's nonce")
	}
	if err := verifyAttestationSignature(dac, elements.Data, attestationChallenge, signature.Data); err != nil {
		return Attestation{}, err
	}
	vendorID, productID, err := dacVIDPID(dac)
	if err != nil {
		return Attestation{}, err
	}
	if expectedVendorID != 0 && vendorID != expectedVendorID {
		return Attestation{}, fmt.Errorf("DAC vendor ID 0x%04X does not match onboarding 0x%04X", vendorID, expectedVendorID)
	}
	if expectedProductID != 0 && productID != expectedProductID {
		return Attestation{}, fmt.Errorf("DAC product ID 0x%04X does not match onboarding 0x%04X", productID, expectedProductID)
	}
	return Attestation{DAC: dac, PAI: pai, VendorID: vendorID, ProductID: productID}, nil
}

func (c Client) certificateChain(ctx context.Context, certificateType uint8) ([]byte, error) {
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandCertificateChainRequest,
		CommandCertificateChainResponse, func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(certificateType), 1)
			writer.EndContainer()
		})
	if err != nil {
		return nil, err
	}
	if result.Fields == nil {
		return nil, errors.New("CertificateChainResponse has no fields")
	}
	certificate, ok := result.Fields.Field(0)
	if !ok {
		return nil, fmt.Errorf("CertificateChainResponse has no certificate (%s)", summarizeFields(*result.Fields))
	}
	if certificate.Type != tlv.TypeBytes {
		return nil, fmt.Errorf("CertificateChainResponse certificate has TLV type %d, want byte string (%s)",
			certificate.Type, summarizeFields(*result.Fields))
	}
	if len(certificate.Data) == 0 {
		return nil, errors.New("CertificateChainResponse contains an empty certificate")
	}
	return append([]byte(nil), certificate.Data...), nil
}

// summarizeFields describes the shape of a malformed response without
// logging certificate bytes or other command payloads.
func summarizeFields(fields im.Value) string {
	children := make([]string, 0, len(fields.Children))
	for _, child := range fields.Children {
		detail := fmt.Sprintf("tag=%s type=%d", child.Tag.String(), child.Type)
		if child.Type == tlv.TypeBytes || child.Type == tlv.TypeString {
			detail += fmt.Sprintf(" length=%d", len(child.Data))
		}
		children = append(children, detail)
	}
	return fmt.Sprintf("container type=%d, fields=[%s]", fields.Type, strings.Join(children, ", "))
}

func attestationNonce(encoded []byte) ([]byte, error) {
	reader := tlv.NewReader(encoded)
	root, err := reader.Next()
	if err != nil || root.Type != tlv.TypeStructure {
		return nil, errors.New("attestation elements are not a TLV structure")
	}
	for {
		element, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if element.Type == tlv.TypeEnd {
			break
		}
		if tag, ok := element.Tag.ContextNumber(); ok && tag == 2 && element.Type == tlv.TypeBytes {
			return append([]byte(nil), element.Data...), nil
		}
	}
	return nil, errors.New("attestation elements have no nonce")
}

var (
	oidMatterVID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 1}
	oidMatterPID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 2}
)

func dacVIDPID(certificate *x509.Certificate) (uint16, uint16, error) {
	var vendor, product uint64
	var vendorFound, productFound bool
	for _, name := range certificate.Subject.Names {
		text, ok := name.Value.(string)
		if !ok {
			continue
		}
		switch {
		case name.Type.Equal(oidMatterVID):
			value, err := strconv.ParseUint(text, 16, 16)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid DAC vendor ID %q", text)
			}
			vendor, vendorFound = value, true
		case name.Type.Equal(oidMatterPID):
			value, err := strconv.ParseUint(text, 16, 16)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid DAC product ID %q", text)
			}
			product, productFound = value, true
		}
	}
	if !vendorFound || !productFound {
		return 0, 0, errors.New("DAC subject has no Matter vendor/product ID")
	}
	return uint16(vendor), uint16(product), nil
}

func equalBytes(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}

func (e CommissioningError) Error() string {
	if e.Debug != "" {
		return fmt.Sprintf("commissioning status 0x%02x: %s", e.Status, e.Debug)
	}
	return fmt.Sprintf("commissioning status 0x%02x", e.Status)
}

func (c Client) ArmFailSafe(ctx context.Context, expirySeconds uint16, breadcrumb uint64) error {
	result, err := c.invoke(ctx, ClusterGeneralCommissioning, CommandArmFailSafe, CommandArmFailSafeResponse,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), uint64(expirySeconds))
			writer.PutUint(tlv.Context(1), breadcrumb)
			writer.EndContainer()
		})
	if err != nil {
		return err
	}
	return commissioningResponse(result)
}

// SetRegulatoryConfig supplies the location class and ISO 3166-1 alpha-2
// country used by radio regulation. "XX" is the Matter controller fallback
// when no user country has been configured.
func (c Client) SetRegulatoryConfig(ctx context.Context, location uint8, country string, breadcrumb uint64) error {
	country = strings.ToUpper(strings.TrimSpace(country))
	if location > RegulatoryIndoorOutdoor {
		return errors.New("invalid Matter regulatory location")
	}
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return errors.New("Matter country code must contain two ASCII letters")
	}
	result, err := c.invoke(ctx, ClusterGeneralCommissioning, CommandSetRegulatoryConfig,
		CommandSetRegulatoryConfigRsp, func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(location), 1)
			writer.PutString(tlv.Context(1), country)
			writer.PutUint(tlv.Context(2), breadcrumb)
			writer.EndContainer()
		})
	if err != nil {
		return err
	}
	return commissioningResponse(result)
}

// ConfigureRegulatory reads the accessory's capability and current setting,
// then preserves that setting when it is configurable. A commissioner must
// not send IndoorOutdoor merely because it does not know which modes the
// accessory supports; Outdoor is the conservative fallback.
func (c Client) ConfigureRegulatory(ctx context.Context, country string, breadcrumb uint64) error {
	reports, err := c.IM.Read(ctx,
		im.ConcreteAttributePath(c.Endpoint, ClusterGeneralCommissioning, AttributeRegulatoryConfig),
		im.ConcreteAttributePath(c.Endpoint, ClusterGeneralCommissioning, AttributeLocationCapability),
	)
	if err != nil {
		return err
	}
	var current, capability uint8
	var haveCurrent, haveCapability bool
	for _, report := range reports {
		if report.Status != nil {
			if !report.Status.OK() {
				return *report.Status
			}
			continue
		}
		if report.Path.Attribute == nil || report.Value.Type != tlv.TypeUint ||
			report.Value.Uint > uint64(RegulatoryIndoorOutdoor) {
			continue
		}
		switch *report.Path.Attribute {
		case AttributeRegulatoryConfig:
			current, haveCurrent = uint8(report.Value.Uint), true
		case AttributeLocationCapability:
			capability, haveCapability = uint8(report.Value.Uint), true
		}
	}
	if !haveCurrent || !haveCapability {
		return errors.New("Matter device did not report its regulatory configuration and capability")
	}
	location := capability
	if capability == RegulatoryIndoorOutdoor {
		location = current
		if location != RegulatoryIndoor && location != RegulatoryOutdoor {
			location = RegulatoryOutdoor
		}
	}
	return c.SetRegulatoryConfig(ctx, location, country, breadcrumb)
}

// CSR asks the device to create its operational key pair. The DAC signature
// binds NOCSRElements (and therefore the new operational public key) to the
// same attested device and PASE session before the PKCS#10 signature is
// accepted.
func (c Client) CSR(ctx context.Context, attestationChallenge []byte, dac *x509.Certificate) (*x509.CertificateRequest, error) {
	if len(attestationChallenge) == 0 || dac == nil {
		return nil, errors.New("CSR verification needs the PASE attestation challenge and DAC")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandCSRRequest, CommandCSRResponse,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), nonce)
			writer.PutBool(tlv.Context(1), false)
			writer.EndContainer()
		})
	if err != nil {
		return nil, err
	}
	if result.Fields == nil {
		return nil, errors.New("CSRResponse has no fields")
	}
	elements, ok := result.Fields.Field(0)
	if !ok || elements.Type != tlv.TypeBytes {
		return nil, errors.New("CSRResponse has no NOCSRElements byte string")
	}
	signature, ok := result.Fields.Field(1)
	if !ok || signature.Type != tlv.TypeBytes {
		return nil, errors.New("CSRResponse has no attestation signature")
	}
	if err := verifyAttestationSignature(dac, elements.Data, attestationChallenge, signature.Data); err != nil {
		return nil, fmt.Errorf("verify CSR attestation: %w", err)
	}
	csrBytes, returnedNonce, err := nocCSRElements(elements.Data)
	if err != nil {
		return nil, err
	}
	if !equalBytes(returnedNonce, nonce) {
		return nil, errors.New("NOCSRElements do not echo Stulp's CSR nonce")
	}
	request, err := x509.ParseCertificateRequest(csrBytes)
	if err != nil {
		return nil, fmt.Errorf("parse device CSR: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return nil, fmt.Errorf("verify device CSR: %w", err)
	}
	return request, nil
}

func (c Client) AddTrustedRoot(ctx context.Context, rootCertificate []byte) error {
	// AddTrustedRoot has no response command. A successful status-only
	// InvokeResponse echoes the request path, so request and expected response
	// command IDs are intentionally both CommandAddTrustedRoot.
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandAddTrustedRoot, CommandAddTrustedRoot,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), rootCertificate)
			writer.EndContainer()
		})
	if err != nil {
		return err
	}
	if !result.Status.OK() {
		return result.Status
	}
	return nil
}

func (c Client) AddNOC(ctx context.Context, noc, icac, ipk []byte, adminSubject uint64, adminVendorID uint16) (uint8, error) {
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandAddNOC, CommandNOCResponse,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), noc)
			if len(icac) > 0 {
				writer.PutBytes(tlv.Context(1), icac)
			}
			writer.PutBytes(tlv.Context(2), ipk)
			writer.PutUint(tlv.Context(3), adminSubject)
			writer.PutUint(tlv.Context(4), uint64(adminVendorID))
			writer.EndContainer()
		})
	if err != nil {
		return 0, err
	}
	if result.Fields == nil {
		return 0, errors.New("NOCResponse has no fields")
	}
	status, err := requiredUint(*result.Fields, 0, 0xFF)
	if err != nil {
		return 0, err
	}
	if status != 0 {
		debug := optionalString(*result.Fields, 2)
		return 0, CommissioningError{Status: uint8(status), Debug: debug}
	}
	fabricIndex, err := requiredUint(*result.Fields, 1, 0xFF)
	if err != nil {
		return 0, err
	}
	return uint8(fabricIndex), nil
}

// RemoveFabric removes this controller's operational credentials from a
// commissioned node. It must run over CASE while the fabric still exists;
// deleting only local state makes later re-commissioning fail because the
// accessory still considers itself a member of the fabric.
func (c Client) RemoveFabric(ctx context.Context, fabricIndex uint8) error {
	if fabricIndex == 0 {
		return errors.New("RemoveFabric needs a non-zero fabric index")
	}
	result, err := c.invoke(ctx, ClusterOperationalCredentials, CommandRemoveFabric, CommandNOCResponse,
		func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(fabricIndex), 1)
			writer.EndContainer()
		})
	if err != nil {
		return err
	}
	if result.Fields == nil {
		return errors.New("RemoveFabric NOCResponse has no fields")
	}
	status, err := requiredUint(*result.Fields, 0, 0xFF)
	if err != nil {
		return err
	}
	if status != 0 {
		return CommissioningError{Status: uint8(status), Debug: optionalString(*result.Fields, 2)}
	}
	return nil
}

func (c Client) Complete(ctx context.Context) error {
	result, err := c.invoke(ctx, ClusterGeneralCommissioning, CommandCommissioningComplete,
		CommandCommissioningCompleteRsp, nil)
	if err != nil {
		return err
	}
	return commissioningResponse(result)
}

func (c Client) invoke(ctx context.Context, cluster, request, response uint32,
	fields func(*tlv.Writer, tlv.Tag)) (im.InvokeResult, error) {
	results, err := c.IM.Invoke(ctx, im.Command{
		Path:   im.CommandPath{Endpoint: c.Endpoint, Cluster: cluster, Command: request},
		Fields: fields,
	})
	if err != nil {
		return im.InvokeResult{}, err
	}
	if len(results) != 1 {
		return im.InvokeResult{}, fmt.Errorf("command 0x%02x returned %d results", request, len(results))
	}
	result := results[0]
	if !result.Status.OK() {
		return im.InvokeResult{}, result.Status
	}
	if result.Path.Endpoint != c.Endpoint || result.Path.Cluster != cluster || result.Path.Command != response {
		return im.InvokeResult{}, fmt.Errorf("unexpected command response path %d/0x%04x/0x%02x",
			result.Path.Endpoint, result.Path.Cluster, result.Path.Command)
	}
	return result, nil
}

func commissioningResponse(result im.InvokeResult) error {
	if result.Fields == nil {
		return errors.New("commissioning response has no fields")
	}
	status, err := requiredUint(*result.Fields, 0, 0xFF)
	if err != nil {
		return err
	}
	if status != 0 {
		return CommissioningError{Status: uint8(status), Debug: optionalString(*result.Fields, 1)}
	}
	return nil
}

// NOCSRElements ::= structure { csr [1] byte string, nonce [2] byte string }
func nocCSRElements(encoded []byte) ([]byte, []byte, error) {
	reader := tlv.NewReader(encoded)
	root, err := reader.Next()
	if err != nil || root.Type != tlv.TypeStructure {
		return nil, nil, errors.New("NOCSRElements is not a TLV structure")
	}
	var csr, nonce []byte
	for {
		element, err := reader.Next()
		if err != nil {
			return nil, nil, fmt.Errorf("decode NOCSRElements: %w", err)
		}
		if element.Type == tlv.TypeEnd {
			break
		}
		tag, contextTag := element.Tag.ContextNumber()
		if !contextTag || element.Type != tlv.TypeBytes {
			continue
		}
		switch tag {
		case 1:
			csr = append([]byte(nil), element.Data...)
		case 2:
			nonce = append([]byte(nil), element.Data...)
		}
	}
	if len(csr) == 0 || len(nonce) != 32 {
		return nil, nil, errors.New("NOCSRElements need a CSR and 32-byte nonce")
	}
	return csr, nonce, nil
}

func verifyAttestationSignature(dac *x509.Certificate, elements, attestationChallenge, signature []byte) error {
	publicKey, ok := dac.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("Matter DAC does not use ECDSA")
	}
	if len(signature) != 64 {
		return errors.New("Matter attestation signature is not 64 bytes")
	}
	signed := append(append([]byte(nil), elements...), attestationChallenge...)
	hash := sha256.Sum256(signed)
	if !ecdsa.Verify(publicKey, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return errors.New("Matter device attestation signature is invalid")
	}
	return nil
}

func requiredUint(value im.Value, number uint8, maximum uint64) (uint64, error) {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeUint {
		return 0, fmt.Errorf("response field %d is missing or not unsigned", number)
	}
	if field.Uint > maximum {
		return 0, fmt.Errorf("response field %d exceeds %d", number, maximum)
	}
	return field.Uint, nil
}

func optionalString(value im.Value, number uint8) string {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeString {
		return ""
	}
	return string(field.Data)
}
