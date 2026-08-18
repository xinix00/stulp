package commissioning

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

func TestCompleteCommissioningCommandFlowAgainstFakeDevice(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	device, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	controller.RetryInterval = 20 * time.Millisecond
	device.RetryInterval = 20 * time.Millisecond

	i2r := bytes.Repeat([]byte{0x31}, 16)
	r2i := bytes.Repeat([]byte{0x42}, 16)
	controllerSession, err := controller.RegisterSession(transport.SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: i2r, InboundKey: r2i, Remote: device.LocalAddr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.RegisterSession(transport.SessionConfig{
		LocalID: 0x2001, PeerID: 0x1001, OutboundKey: r2i, InboundKey: i2r, Remote: controller.LocalAddr(),
	}); err != nil {
		t.Fatal(err)
	}

	challenge := bytes.Repeat([]byte{0xA5}, 16)
	fake := newCommissioningDevice(t, challenge)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- fake.serve(ctx, device) }()

	client := Client{IM: im.Client{Transport: controller, Session: controllerSession}}
	if err := client.ArmFailSafe(ctx, 120, 1); err != nil {
		t.Fatalf("ArmFailSafe: %v", err)
	}
	if err := client.SetRegulatoryConfig(ctx, RegulatoryIndoorOutdoor, "xx", 2); err != nil {
		t.Fatalf("SetRegulatoryConfig: %v", err)
	}
	attestation, err := client.Attest(ctx, challenge, 0xFFF1, 0x8000)
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	csr, err := client.CSR(ctx, challenge, attestation.DAC)
	if err != nil {
		t.Fatalf("CSR: %v", err)
	}
	if !csr.PublicKey.(*ecdsa.PublicKey).Equal(&fake.operationalKey.PublicKey) {
		t.Fatal("CSR returned a different operational public key")
	}
	if err := client.AddTrustedRoot(ctx, fake.trustedRoot); err != nil {
		t.Fatalf("AddTrustedRoot: %v", err)
	}
	fabricIndex, err := client.AddNOC(ctx, fake.noc, nil, fake.ipk, 0x1122334455667788, 0xFFF1)
	if err != nil || fabricIndex != 7 {
		t.Fatalf("AddNOC: fabric=%d err=%v", fabricIndex, err)
	}
	if err := client.Complete(ctx); err != nil {
		t.Fatalf("CommissioningComplete: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}

	want := []im.CommandPath{
		{Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CommandArmFailSafe},
		{Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CommandSetRegulatoryConfig},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandCertificateChainRequest},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandCertificateChainRequest},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandAttestationRequest},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandCSRRequest},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandAddTrustedRoot},
		{Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CommandAddNOC},
		{Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CommandCommissioningComplete},
	}
	if len(fake.seen) != len(want) {
		t.Fatalf("device saw %d commands, want %d: %#v", len(fake.seen), len(want), fake.seen)
	}
	for index := range want {
		if fake.seen[index] != want[index] {
			t.Fatalf("command %d = %#v, want %#v", index, fake.seen[index], want[index])
		}
	}
	if got, want := fake.certificateTypes, []uint8{CertificateTypePAI, CertificateTypeDAC}; !bytes.Equal(got, want) {
		t.Fatalf("certificate request order = %v, want PAI then DAC (%v)", got, want)
	}
}

type commissioningDevice struct {
	challenge        []byte
	pai, dac         *x509.Certificate
	dacKey           *ecdsa.PrivateKey
	operationalKey   *ecdsa.PrivateKey
	csr              []byte
	trustedRoot      []byte
	noc              []byte
	ipk              []byte
	seen             []im.CommandPath
	certificateTypes []uint8
}

func newCommissioningDevice(t *testing.T, challenge []byte) *commissioningDevice {
	t.Helper()
	paiKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dacKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	paiTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake Matter PAI"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	paiDER, err := x509.CreateCertificate(rand.Reader, paiTemplate, paiTemplate, &paiKey.PublicKey, paiKey)
	if err != nil {
		t.Fatal(err)
	}
	pai, err := x509.ParseCertificate(paiDER)
	if err != nil {
		t.Fatal(err)
	}
	dacTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "Fake Matter DAC", ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: asn1.ObjectIdentifier(oidMatterVID), Value: "FFF1"},
			{Type: asn1.ObjectIdentifier(oidMatterPID), Value: "8000"},
		}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
	}
	dacDER, err := x509.CreateCertificate(rand.Reader, dacTemplate, pai, &dacKey.PublicKey, paiKey)
	if err != nil {
		t.Fatal(err)
	}
	dac, err := x509.ParseCertificate(dacDER)
	if err != nil {
		t.Fatal(err)
	}
	operationalKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, operationalKey)
	if err != nil {
		t.Fatal(err)
	}
	return &commissioningDevice{
		challenge: append([]byte(nil), challenge...), pai: pai, dac: dac, dacKey: dacKey,
		operationalKey: operationalKey, csr: csr, trustedRoot: []byte("compact-root"),
		noc: []byte("compact-device-noc"), ipk: bytes.Repeat([]byte{0x55}, 16),
	}
}

func (f *commissioningDevice) serve(ctx context.Context, node *transport.Node) error {
	for range 9 {
		exchange, err := node.Accept(ctx)
		if err != nil {
			return err
		}
		opcode, payload, err := exchange.Receive(ctx)
		if err != nil {
			exchange.Close()
			return err
		}
		if opcode != im.OpcodeInvokeRequest {
			exchange.Close()
			return fmt.Errorf("fake device received opcode 0x%02x", opcode)
		}
		commands, err := im.DecodeInvokeRequest(payload)
		if err != nil {
			exchange.Close()
			return fmt.Errorf("decode fake-device request: %w", err)
		}
		if len(commands) != 1 {
			exchange.Close()
			return fmt.Errorf("fake device received %d commands", len(commands))
		}
		response, err := f.respond(commands[0])
		if err == nil {
			var encoded []byte
			encoded, err = im.EncodeInvokeResponseMessage([]im.CommandResponse{response}, false, false)
			if err == nil {
				err = exchange.Send(ctx, im.OpcodeInvokeResponse, encoded)
			}
		}
		exchange.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *commissioningDevice) respond(command im.InvokeRequestCommand) (im.CommandResponse, error) {
	f.seen = append(f.seen, command.Path)
	response := im.CommandResponse{Path: command.Path}
	switch {
	case command.Path.Cluster == ClusterGeneralCommissioning && command.Path.Command == CommandArmFailSafe:
		if expiry, _ := requiredUint(command.Fields, 0, 0xFFFF); expiry != 120 {
			return response, fmt.Errorf("ArmFailSafe expiry = %d", expiry)
		}
		response.Path.Command = CommandArmFailSafeResponse
		response.Fields = statusFields(0, "")

	case command.Path.Cluster == ClusterGeneralCommissioning && command.Path.Command == CommandSetRegulatoryConfig:
		location, _ := requiredUint(command.Fields, 0, 0xFF)
		country, countryOK := command.Fields.Field(1)
		breadcrumb, _ := requiredUint(command.Fields, 2, ^uint64(0))
		if location != uint64(RegulatoryIndoorOutdoor) || !countryOK || country.Type != tlv.TypeString ||
			string(country.Data) != "XX" || breadcrumb != 2 {
			return response, fmt.Errorf("invalid SetRegulatoryConfig fields: %#v", command.Fields)
		}
		response.Path.Command = CommandSetRegulatoryConfigRsp
		response.Fields = statusFields(0, "")

	case command.Path.Cluster == ClusterOperationalCredentials && command.Path.Command == CommandAttestationRequest:
		nonce, err := byteField(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		elements, err := attestationElements(nonce)
		if err != nil {
			return response, err
		}
		signature, err := signElements(f.dacKey, elements, f.challenge)
		if err != nil {
			return response, err
		}
		response.Path.Command = CommandAttestationResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), elements)
			writer.PutBytes(tlv.Context(1), signature)
			writer.EndContainer()
		}

	case command.Path.Cluster == ClusterOperationalCredentials && command.Path.Command == CommandCertificateChainRequest:
		certificateType, err := requiredUint(command.Fields, 0, 2)
		if err != nil {
			return response, err
		}
		var certificate []byte
		if certificateType == uint64(CertificateTypeDAC) {
			certificate = f.dac.Raw
		} else if certificateType == uint64(CertificateTypePAI) {
			certificate = f.pai.Raw
		} else {
			return response, fmt.Errorf("certificate type = %d", certificateType)
		}
		f.certificateTypes = append(f.certificateTypes, uint8(certificateType))
		response.Path.Command = CommandCertificateChainResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), certificate)
			writer.EndContainer()
		}

	case command.Path.Cluster == ClusterOperationalCredentials && command.Path.Command == CommandCSRRequest:
		nonce, err := byteField(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		isForUpdate, ok := command.Fields.Field(1)
		if !ok || isForUpdate.Type != tlv.TypeBool || isForUpdate.Bool {
			return response, errors.New("CSR isForUpdate must be false")
		}
		var elementsWriter tlv.Writer
		elementsWriter.StartStructure(tlv.Anonymous())
		elementsWriter.PutBytes(tlv.Context(1), f.csr)
		elementsWriter.PutBytes(tlv.Context(2), nonce)
		elementsWriter.EndContainer()
		elements, err := elementsWriter.Bytes()
		if err != nil {
			return response, err
		}
		signature, err := signElements(f.dacKey, elements, f.challenge)
		if err != nil {
			return response, err
		}
		response.Path.Command = CommandCSRResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), elements)
			writer.PutBytes(tlv.Context(1), signature)
			writer.EndContainer()
		}

	case command.Path.Cluster == ClusterOperationalCredentials && command.Path.Command == CommandAddTrustedRoot:
		root, err := byteField(command.Fields, 0, len(f.trustedRoot))
		if err != nil || !bytes.Equal(root, f.trustedRoot) {
			return response, errors.New("AddTrustedRoot received a different root")
		}
		// There is deliberately no response command; success is a status-only
		// response carrying this same request path.
		status := im.Status{Global: im.StatusSuccess}
		response.Status = &status

	case command.Path.Cluster == ClusterOperationalCredentials && command.Path.Command == CommandAddNOC:
		noc, err := byteField(command.Fields, 0, len(f.noc))
		if err != nil || !bytes.Equal(noc, f.noc) {
			return response, errors.New("AddNOC received a different NOC")
		}
		ipk, err := byteField(command.Fields, 2, 16)
		if err != nil || !bytes.Equal(ipk, f.ipk) {
			return response, errors.New("AddNOC received a different IPK")
		}
		if subject, _ := requiredUint(command.Fields, 3, ^uint64(0)); subject != 0x1122334455667788 {
			return response, fmt.Errorf("AddNOC admin subject = %X", subject)
		}
		if vendor, _ := requiredUint(command.Fields, 4, 0xFFFF); vendor != 0xFFF1 {
			return response, fmt.Errorf("AddNOC vendor = %X", vendor)
		}
		response.Path.Command = CommandNOCResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.PutUint(tlv.Context(1), 7)
			writer.EndContainer()
		}

	case command.Path.Cluster == ClusterGeneralCommissioning && command.Path.Command == CommandCommissioningComplete:
		response.Path.Command = CommandCommissioningCompleteRsp
		response.Fields = statusFields(0, "")

	default:
		return response, fmt.Errorf("unexpected commissioning command %#v", command.Path)
	}
	return response, nil
}

func statusFields(status uint8, debug string) func(*tlv.Writer, tlv.Tag) {
	return func(writer *tlv.Writer, tag tlv.Tag) {
		writer.StartStructure(tag)
		writer.PutUint(tlv.Context(0), uint64(status))
		writer.PutString(tlv.Context(1), debug)
		writer.EndContainer()
	}
}

func byteField(value im.Value, number uint8, length int) ([]byte, error) {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeBytes || len(field.Data) != length {
		return nil, fmt.Errorf("field %d is not a %d-byte string", number, length)
	}
	return field.Data, nil
}

func attestationElements(nonce []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUint(tlv.Context(0), 1)
	writer.PutBytes(tlv.Context(2), nonce)
	writer.EndContainer()
	return writer.Bytes()
}

func signElements(key *ecdsa.PrivateKey, elements, challenge []byte) ([]byte, error) {
	hash := sha256.Sum256(append(append([]byte(nil), elements...), challenge...))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return nil, err
	}
	result := make([]byte, 64)
	r.FillBytes(result[:32])
	s.FillBytes(result[32:])
	return result, nil
}
