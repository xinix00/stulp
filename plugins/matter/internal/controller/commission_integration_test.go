package controller

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
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/casesession"
	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const (
	fakePasscode = 20202021
	fakeNodeID   = 0x10000
)

var (
	fakeVIDOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 1}
	fakePIDOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 37244, 2, 2}
)

// This is the hardware boundary in one hermetic test: a stateful device
// answers PASE, every commissioning command, CASE, descriptor/cluster reads,
// an operational command, a controller restart, a fresh CASE, another command
// and RemoveFabric. Only UDP packets cross between controller and fake device.
func TestCommissionControlAndReconnectAgainstStatefulMatterDevice(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	database := newBacking()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Eén backing voor beide controllers: een herstarte plugin krijgt zijn eigen
	// state terug, en dat is precies wat hier bewezen moet worden.
	backing := database
	controller, err := New(ctx, backing, logger)
	if err != nil {
		t.Fatal(err)
	}
	controller.node.RetryInterval = 20 * time.Millisecond
	device, err := newStatefulMatterDevice(t, logger)
	if err != nil {
		controller.Close()
		t.Fatal(err)
	}
	defer device.node.Close()
	serverErr := make(chan error, 1)
	go func() { serverErr <- device.serve(ctx, controller.fabric) }()

	added, err := commissionAndStore(ctx, t, controller, database, CommissionRequest{
		Code: "34970112332", Address: device.node.LocalAddr().String(),
	})
	if err != nil {
		controller.Close()
		t.Fatalf("Commission: %v", err)
	}
	if len(added) != 1 {
		controller.Close()
		t.Fatalf("commissioned devices = %d, want 1", len(added))
	}
	lamp := added[0]
	if lamp.Name != "Fake Lamp" || lamp.Class != "light" || !slices.Contains(lamp.Capabilities, "onoff") {
		controller.Close()
		t.Fatalf("commissioned device = %#v", lamp)
	}
	if state, ok := lamp.State["onoff"].(bool); !ok || state {
		controller.Close()
		t.Fatalf("initial onoff state = %#v", lamp.State["onoff"])
	}
	waitForSubscription(t, ctx, controller, fakeNodeID)
	waitForMatterEvent(t, ctx, database, lamp.ID)
	if err := controller.SetCapability(ctx, lamp.ID, "onoff", true); err != nil {
		controller.Close()
		t.Fatalf("first operational command: %v", err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(ctx, backing, logger)
	if err != nil {
		t.Fatal(err)
	}
	restarted.node.RetryInterval = 20 * time.Millisecond
	defer restarted.Close()
	waitForSubscription(t, ctx, restarted, fakeNodeID)
	if err := restarted.SetCapability(ctx, lamp.ID, "onoff", false); err != nil {
		t.Fatalf("command after controller restart: %v", err)
	}
	if err := restarted.DeleteDevice(ctx, lamp.ID); err != nil {
		t.Fatalf("remove fabric and device: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if _, err := database.Device(ctx, lamp.ID); err == nil {
		t.Fatal("device remained in the document after RemoveFabric")
	}
	if device.on {
		t.Fatal("fake device did not execute the command after reconnect")
	}
	if !device.removed {
		t.Fatal("controller deleted local state without removing its fabric from the accessory")
	}
}

func waitForSubscription(t *testing.T, ctx context.Context, controller *Controller, nodeID uint64) {
	t.Helper()
	for {
		controller.subMu.RLock()
		_, active := controller.subscriptions[nodeID]
		controller.subMu.RUnlock()
		if active {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("Matter subscription was not established")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForMatterEvent(t *testing.T, ctx context.Context, database *fakeBacking, deviceID string) {
	t.Helper()
	for {
		device, err := database.Device(ctx, deviceID)
		if err != nil {
			t.Fatal(err)
		}
		if device.Store["matter.lastEventNumber"] != nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("unsolicited Matter event was not persisted")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type statefulMatterDevice struct {
	node           *transport.Node
	pase           *pase.Device
	pai, dac       *x509.Certificate
	dacKey         *ecdsa.PrivateKey
	operationalKey *ecdsa.PrivateKey
	csr            []byte
	challenge      []byte
	noc            []byte
	on             bool
	removed        bool
	eventNumber    uint64
}

func newStatefulMatterDevice(t *testing.T, logger *slog.Logger) (*statefulMatterDevice, error) {
	t.Helper()
	parameters, err := pase.DefaultParameters()
	if err != nil {
		return nil, err
	}
	paseDevice, err := pase.NewDevice(fakePasscode, parameters)
	if err != nil {
		return nil, err
	}
	node, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		return nil, err
	}
	node.RetryInterval = 20 * time.Millisecond
	pai, dac, dacKey, err := fakeAttestationChain()
	if err != nil {
		node.Close()
		return nil, err
	}
	operationalKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		node.Close()
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, operationalKey)
	if err != nil {
		node.Close()
		return nil, err
	}
	return &statefulMatterDevice{
		node: node, pase: paseDevice, pai: pai, dac: dac, dacKey: dacKey,
		operationalKey: operationalKey, csr: csr,
	}, nil
}

func fakeAttestationChain() (*x509.Certificate, *x509.Certificate, *ecdsa.PrivateKey, error) {
	paiKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	dacKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now()
	paiTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Stulp test PAI"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	paiDER, err := x509.CreateCertificate(rand.Reader, paiTemplate, paiTemplate, &paiKey.PublicKey, paiKey)
	if err != nil {
		return nil, nil, nil, err
	}
	pai, err := x509.ParseCertificate(paiDER)
	if err != nil {
		return nil, nil, nil, err
	}
	dacTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "Stulp test DAC", ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: fakeVIDOID, Value: "FFF1"}, {Type: fakePIDOID, Value: "8000"},
		}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
	}
	dacDER, err := x509.CreateCertificate(rand.Reader, dacTemplate, pai, &dacKey.PublicKey, paiKey)
	if err != nil {
		return nil, nil, nil, err
	}
	dac, err := x509.ParseCertificate(dacDER)
	return pai, dac, dacKey, err
}

func (d *statefulMatterDevice) serve(ctx context.Context, fabric *credentials.Fabric) error {
	paseExchange, err := d.node.Accept(ctx)
	if err != nil {
		return err
	}
	paseSession, err := d.pase.Serve(ctx, paseExchange)
	remote := paseExchange.Remote()
	paseExchange.Close()
	if err != nil {
		return err
	}
	d.challenge = append([]byte(nil), paseSession.Keys.AttestationChallenge...)
	secure, err := d.node.RegisterSession(transport.SessionConfig{
		LocalID: paseSession.LocalSessionID, PeerID: paseSession.PeerSessionID,
		OutboundKey: paseSession.Keys.R2I, InboundKey: paseSession.Keys.I2R, Remote: remote,
	})
	if err != nil {
		return err
	}
	if err := d.serveCommissioning(ctx, secure, fabric); err != nil {
		return err
	}
	d.node.RemoveSession(secure.LocalID)

	first, err := casesession.Accept(ctx, d.node, casesession.ResponderConfig{
		Fabric: fabric, LocalNodeID: fakeNodeID, PrivateKey: d.operationalKey, NOC: d.noc,
	})
	if err != nil {
		return err
	}
	if err := d.serveOperational(ctx, first, true); err != nil {
		return err
	}
	d.node.RemoveSession(first.LocalID)
	second, err := casesession.Accept(ctx, d.node, casesession.ResponderConfig{
		Fabric: fabric, LocalNodeID: fakeNodeID, PrivateKey: d.operationalKey, NOC: d.noc,
	})
	if err != nil {
		return err
	}
	return d.serveOperational(ctx, second, false)
}

func (d *statefulMatterDevice) serveCommissioning(ctx context.Context, _ *transport.SecureSession,
	fabric *credentials.Fabric) error {
	commandsSeen := 0
	for commandsSeen < 8 {
		exchange, err := d.node.Accept(ctx)
		if err != nil {
			return err
		}
		opcode, payload, err := exchange.Receive(ctx)
		if err == nil {
			switch opcode {
			case im.OpcodeReadRequest:
				err = d.respondRead(ctx, exchange, payload)
			case im.OpcodeInvokeRequest:
				var commands []im.InvokeRequestCommand
				commands, err = im.DecodeInvokeRequest(payload)
				if err == nil && len(commands) != 1 {
					err = fmt.Errorf("commissioning received %d commands", len(commands))
				}
				var response im.CommandResponse
				if err == nil {
					response, err = d.commissioningResponse(commands[0], fabric)
				}
				if err == nil {
					var encoded []byte
					encoded, err = im.EncodeInvokeResponseMessage([]im.CommandResponse{response}, false, false)
					if err == nil {
						err = exchange.Send(ctx, im.OpcodeInvokeResponse, encoded)
					}
				}
				commandsSeen++
			default:
				err = fmt.Errorf("commissioning received opcode 0x%02x", opcode)
			}
		}
		exchange.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *statefulMatterDevice) commissioningResponse(command im.InvokeRequestCommand,
	fabric *credentials.Fabric) (im.CommandResponse, error) {
	response := im.CommandResponse{Path: command.Path}
	switch {
	case command.Path.Cluster == commissioning.ClusterGeneralCommissioning && command.Path.Command == commissioning.CommandArmFailSafe:
		response.Path.Command = commissioning.CommandArmFailSafeResponse
		response.Fields = fakeCommissioningStatus
	case command.Path.Cluster == commissioning.ClusterGeneralCommissioning && command.Path.Command == commissioning.CommandSetRegulatoryConfig:
		location, locationOK := command.Fields.Field(0)
		country, countryOK := command.Fields.Field(1)
		if !locationOK || location.Type != tlv.TypeUint || location.Uint != uint64(commissioning.RegulatoryOutdoor) ||
			!countryOK || country.Type != tlv.TypeString || string(country.Data) != "XX" {
			return response, errors.New("SetRegulatoryConfig carried invalid fields")
		}
		response.Path.Command = commissioning.CommandSetRegulatoryConfigRsp
		response.Fields = fakeCommissioningStatus
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandAttestationRequest:
		nonce, err := fakeBytes(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		elements, err := fakeAttestationElements(nonce)
		if err != nil {
			return response, err
		}
		signature, err := fakeSign(d.dacKey, elements, d.challenge)
		if err != nil {
			return response, err
		}
		response.Path.Command = commissioning.CommandAttestationResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), elements)
			writer.PutBytes(tlv.Context(1), signature)
			writer.EndContainer()
		}
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandCertificateChainRequest:
		kind, ok := command.Fields.Field(0)
		if !ok || kind.Type != tlv.TypeUint {
			return response, errors.New("CertificateChainRequest has no certificate type")
		}
		certificate := d.dac.Raw
		if kind.Uint == uint64(commissioning.CertificateTypePAI) {
			certificate = d.pai.Raw
		} else if kind.Uint != uint64(commissioning.CertificateTypeDAC) {
			return response, fmt.Errorf("unexpected certificate type %d", kind.Uint)
		}
		response.Path.Command = commissioning.CommandCertificateChainResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), certificate)
			writer.EndContainer()
		}
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandCSRRequest:
		nonce, err := fakeBytes(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		var elementsWriter tlv.Writer
		elementsWriter.StartStructure(tlv.Anonymous())
		elementsWriter.PutBytes(tlv.Context(1), d.csr)
		elementsWriter.PutBytes(tlv.Context(2), nonce)
		elementsWriter.EndContainer()
		elements, err := elementsWriter.Bytes()
		if err != nil {
			return response, err
		}
		signature, err := fakeSign(d.dacKey, elements, d.challenge)
		if err != nil {
			return response, err
		}
		response.Path.Command = commissioning.CommandCSRResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), elements)
			writer.PutBytes(tlv.Context(1), signature)
			writer.EndContainer()
		}
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandAddTrustedRoot:
		root, err := fakeBytes(command.Fields, 0, -1)
		want, wantErr := fabric.RootMatterCertificate()
		if err != nil || wantErr != nil || !bytes.Equal(root, want) {
			return response, errors.New("AddTrustedRoot did not carry the controller root")
		}
		status := im.Status{Global: im.StatusSuccess}
		response.Status = &status
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandAddNOC:
		noc, err := fakeBytes(command.Fields, 0, -1)
		ipk, ipkErr := fakeBytes(command.Fields, 2, 16)
		if err != nil || ipkErr != nil || !bytes.Equal(ipk, fabric.IPK) {
			return response, errors.New("AddNOC carried invalid credentials")
		}
		d.noc = append([]byte(nil), noc...)
		response.Path.Command = commissioning.CommandNOCResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.PutUint(tlv.Context(1), 7)
			writer.EndContainer()
		}
	default:
		return response, fmt.Errorf("unexpected commissioning command %#v", command.Path)
	}
	return response, nil
}

func (d *statefulMatterDevice) serveOperational(ctx context.Context, session *transport.SecureSession,
	waitForControl bool) error {
	for {
		exchange, err := d.node.Accept(ctx)
		if err != nil {
			return err
		}
		opcode, payload, err := exchange.Receive(ctx)
		event := ""
		switch {
		case err != nil:
		case opcode == im.OpcodeReadRequest:
			err = d.respondRead(ctx, exchange, payload)
		case opcode == im.OpcodeSubscribeRequest:
			err = d.respondSubscribe(ctx, exchange, session, payload)
		case opcode == im.OpcodeInvokeRequest:
			event, err = d.respondOperationalInvoke(ctx, exchange, payload)
		default:
			err = fmt.Errorf("operational session received opcode 0x%02x", opcode)
		}
		exchange.Close()
		if err != nil {
			return err
		}
		if event == "remove" || (event == "control" && waitForControl) {
			return nil
		}
	}
}

func (d *statefulMatterDevice) respondSubscribe(ctx context.Context, exchange *transport.Exchange,
	session *transport.SecureSession, payload []byte) error {
	request, err := im.DecodeSubscribeRequest(payload)
	if err != nil {
		return err
	}
	reports := make([]im.AttributeData, 0, len(request.Attributes))
	for _, path := range request.Attributes {
		value, valueErr := d.attribute(path)
		if valueErr != nil {
			return valueErr
		}
		reports = append(reports, im.AttributeData{Path: path, Value: value})
	}
	subscriptionID := uint32(91)
	priming, err := im.EncodeReportDataMessage(&subscriptionID, reports, nil, false, false)
	if err != nil {
		return err
	}
	if err := exchange.Send(ctx, im.OpcodeReportData, priming); err != nil {
		return err
	}
	opcode, statusBytes, err := exchange.Receive(ctx)
	if err != nil || opcode != im.OpcodeStatusResponse {
		return errors.Join(err, fmt.Errorf("subscription priming status opcode 0x%02x", opcode))
	}
	status, err := im.DecodeStatusResponse(statusBytes)
	if err != nil || !status.OK() {
		return errors.Join(err, status)
	}
	response, err := im.EncodeSubscribeResponse(subscriptionID, 30)
	if err != nil {
		return err
	}
	if err := exchange.Send(ctx, im.OpcodeSubscribeResponse, response); err != nil {
		return err
	}

	// Exercise the defining property of a subscription: the event arrives on
	// a fresh peer-initiated encrypted exchange, not as a reply to polling.
	d.eventNumber++
	endpoint, cluster, eventID := uint16(1), switchCluster, uint32(1)
	systemTimestamp := uint64(time.Second / time.Millisecond)
	eventReport, err := im.EncodeReportDataMessage(&subscriptionID, nil, []im.EventData{{
		Path:   im.EventPath{Endpoint: &endpoint, Cluster: &cluster, Event: &eventID},
		Number: d.eventNumber, Priority: 1, SystemTimestamp: &systemTimestamp,
		Value: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 1)
			writer.EndContainer()
		},
	}}, false, false)
	if err != nil {
		return err
	}
	for range 5 {
		reportExchange, initiateErr := d.node.InitiateSecure(session, message.ProtocolInteractionModel)
		if initiateErr != nil {
			return initiateErr
		}
		sendErr := reportExchange.Send(ctx, im.OpcodeReportData, eventReport)
		var reportStatus im.Status
		if sendErr == nil {
			opcode, statusBytes, sendErr = reportExchange.Receive(ctx)
			if sendErr == nil && opcode != im.OpcodeStatusResponse {
				sendErr = fmt.Errorf("event report received opcode 0x%02x", opcode)
			}
			if sendErr == nil {
				reportStatus, sendErr = im.DecodeStatusResponse(statusBytes)
			}
		}
		reportExchange.Close()
		if sendErr != nil {
			return sendErr
		}
		if reportStatus.OK() {
			return nil
		}
		if reportStatus.Global != im.StatusInvalidSubscription {
			return reportStatus
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("controller never activated the accepted Matter subscription")
}

func (d *statefulMatterDevice) respondRead(ctx context.Context, exchange *transport.Exchange, payload []byte) error {
	request, err := im.DecodeReadRequest(payload)
	if err != nil {
		return err
	}
	reports := make([]im.AttributeData, 0, len(request.Paths))
	for _, path := range request.Paths {
		// A real node expands a wildcard cluster path into one report per
		// attribute it implements, and answers nothing at all for a cluster it
		// does not have. Diagnostics reads rely on both behaviours.
		if path.Attribute == nil {
			reports = append(reports, d.clusterAttributes(path)...)
			continue
		}
		value, err := d.attribute(path)
		if err != nil {
			return err
		}
		reports = append(reports, im.AttributeData{Path: path, Value: value})
	}
	encoded, err := im.EncodeReportDataMessage(nil, reports, nil, false, false)
	if err != nil {
		return err
	}
	if err := exchange.Send(ctx, im.OpcodeReportData, encoded); err != nil {
		return err
	}
	opcode, statusBytes, err := exchange.Receive(ctx)
	if err != nil {
		return err
	}
	if opcode != im.OpcodeStatusResponse {
		return fmt.Errorf("read response received opcode 0x%02x instead of status", opcode)
	}
	status, err := im.DecodeStatusResponse(statusBytes)
	if err != nil || !status.OK() {
		return errors.Join(err, status)
	}
	return exchange.Acknowledge()
}

// clusterAttributes expands a wildcard cluster read. The fake node is a Thread
// device, so it implements Basic Information, General Diagnostics and Thread
// Network Diagnostics, and answers nothing for the Wi-Fi cluster.
func (d *statefulMatterDevice) clusterAttributes(path im.AttributePath) []im.AttributeData {
	if path.Endpoint == nil || *path.Endpoint != 0 || path.Cluster == nil {
		return nil
	}
	putString := func(value string) func(*tlv.Writer, tlv.Tag) {
		return func(writer *tlv.Writer, tag tlv.Tag) { writer.PutString(tag, value) }
	}
	putUint := func(value uint64) func(*tlv.Writer, tlv.Tag) {
		return func(writer *tlv.Writer, tag tlv.Tag) { writer.PutUint(tag, value) }
	}
	var values map[uint32]func(*tlv.Writer, tlv.Tag)
	switch *path.Cluster {
	case basicCluster:
		values = map[uint32]func(*tlv.Writer, tlv.Tag){
			1: putString("Stulp Labs"), 3: putString("Fake Lamp"),
			8: putString("rev-B"), 0x0A: putString("1.4.2"), 0x0F: putString("SN-0001"),
		}
	case generalDiagnosticsCluster:
		values = map[uint32]func(*tlv.Writer, tlv.Tag){
			1: putUint(3), 2: putUint(93_784), 3: putUint(1200), 4: putUint(3),
		}
	case threadDiagnosticsCluster:
		values = map[uint32]func(*tlv.Writer, tlv.Tag){
			0: putUint(15), 1: putUint(6), 2: putString("Stulp-Thread"), 3: putUint(0x1A2B),
			0x0D: putUint(12),
			7: func(writer *tlv.Writer, tag tlv.Tag) {
				writer.StartArray(tag)
				writer.StartStructure(tlv.Anonymous())
				writer.PutUint(tlv.Context(0), 0xA1B2C3D4E5F60718)
				writer.PutUint(tlv.Context(1), 37)
				writer.PutUint(tlv.Context(2), 0x4401)
				writer.PutUint(tlv.Context(5), 180)
				writer.PutInt(tlv.Context(6), -62)
				writer.PutInt(tlv.Context(7), -58)
				writer.PutUint(tlv.Context(8), 2)
				writer.PutUint(tlv.Context(9), 1)
				writer.PutBool(tlv.Context(10), false)
				writer.PutBool(tlv.Context(11), false)
				writer.PutBool(tlv.Context(13), true)
				writer.EndContainer()
				writer.EndContainer()
			},
		}
	default:
		return nil
	}
	numbers := make([]uint32, 0, len(values))
	for number := range values {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(left, right int) bool { return numbers[left] < numbers[right] })
	reports := make([]im.AttributeData, 0, len(numbers))
	for _, number := range numbers {
		reports = append(reports, im.AttributeData{
			Path:  im.ConcreteAttributePath(*path.Endpoint, *path.Cluster, number),
			Value: values[number],
		})
	}
	return reports
}

func (d *statefulMatterDevice) attribute(path im.AttributePath) (func(*tlv.Writer, tlv.Tag), error) {
	if path.Endpoint == nil || path.Cluster == nil || path.Attribute == nil {
		return nil, errors.New("fake device only accepts concrete attribute paths")
	}
	endpoint, cluster, attribute := *path.Endpoint, *path.Cluster, *path.Attribute
	putString := func(value string) func(*tlv.Writer, tlv.Tag) {
		return func(writer *tlv.Writer, tag tlv.Tag) { writer.PutString(tag, value) }
	}
	putUint := func(value uint64) func(*tlv.Writer, tlv.Tag) {
		return func(writer *tlv.Writer, tag tlv.Tag) { writer.PutUint(tag, value) }
	}
	putArray := func(values ...uint64) func(*tlv.Writer, tlv.Tag) {
		return func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartArray(tag)
			for _, value := range values {
				writer.PutUint(tlv.Anonymous(), value)
			}
			writer.EndContainer()
		}
	}
	switch {
	case endpoint == 0 && cluster == commissioning.ClusterGeneralCommissioning &&
		attribute == commissioning.AttributeRegulatoryConfig:
		return putUint(uint64(commissioning.RegulatoryOutdoor)), nil
	case endpoint == 0 && cluster == commissioning.ClusterGeneralCommissioning &&
		attribute == commissioning.AttributeLocationCapability:
		return putUint(uint64(commissioning.RegulatoryIndoorOutdoor)), nil
	case endpoint == 0 && cluster == basicCluster && attribute == 1:
		return putString("Stulp Labs"), nil
	case endpoint == 0 && cluster == basicCluster && attribute == 2:
		return putUint(0xFFF1), nil
	case endpoint == 0 && cluster == basicCluster && attribute == 3:
		return putString("Fake Lamp"), nil
	case endpoint == 0 && cluster == basicCluster && attribute == 4:
		return putUint(0x8000), nil
	case endpoint == 0 && cluster == basicCluster && attribute == 5:
		return putString("Desk"), nil
	case endpoint == 0 && cluster == descriptorCluster && attribute == 3:
		return func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartArray(tag)
			writer.PutUint(tlv.Anonymous(), 1)
			writer.EndContainer()
		}, nil
	case endpoint == 1 && cluster == descriptorCluster && attribute == 0:
		return func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartArray(tag)
			writer.StartStructure(tlv.Anonymous())
			writer.PutUint(tlv.Context(0), 0x0100)
			writer.PutUint(tlv.Context(1), 1)
			writer.EndContainer()
			writer.EndContainer()
		}, nil
	case endpoint == 1 && cluster == descriptorCluster && attribute == 1:
		return func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartArray(tag)
			writer.PutUint(tlv.Anonymous(), uint64(onOffCluster))
			writer.EndContainer()
		}, nil
	case endpoint == 1 && cluster == onOffCluster && attribute == 0:
		return func(writer *tlv.Writer, tag tlv.Tag) { writer.PutBool(tag, d.on) }, nil
	case endpoint == 1 && cluster == onOffCluster && attribute == generatedCommandListAttribute:
		return putArray(), nil
	case endpoint == 1 && cluster == onOffCluster && attribute == acceptedCommandListAttribute:
		return putArray(0, 1, 2), nil
	case endpoint == 1 && cluster == onOffCluster && attribute == eventListAttribute:
		return putArray(), nil
	case endpoint == 1 && cluster == onOffCluster && attribute == attributeListAttribute:
		return putArray(0, uint64(generatedCommandListAttribute), uint64(acceptedCommandListAttribute), uint64(eventListAttribute),
			uint64(attributeListAttribute), uint64(featureMapAttribute), uint64(clusterRevisionAttribute)), nil
	case endpoint == 1 && cluster == onOffCluster && attribute == featureMapAttribute:
		return putUint(0), nil
	case endpoint == 1 && cluster == onOffCluster && attribute == clusterRevisionAttribute:
		return putUint(6), nil
	default:
		return nil, fmt.Errorf("unexpected attribute %d/0x%04x/0x%04x", endpoint, cluster, attribute)
	}
}

func (d *statefulMatterDevice) respondOperationalInvoke(ctx context.Context, exchange *transport.Exchange,
	payload []byte) (string, error) {
	commands, err := im.DecodeInvokeRequest(payload)
	if err != nil || len(commands) != 1 {
		return "", errors.Join(err, fmt.Errorf("operational invoke has %d commands", len(commands)))
	}
	command := commands[0]
	response := im.CommandResponse{Path: command.Path}
	event := ""
	switch {
	case command.Path.Endpoint == 0 && command.Path.Cluster == commissioning.ClusterGeneralCommissioning &&
		command.Path.Command == commissioning.CommandCommissioningComplete:
		response.Path.Command = commissioning.CommandCommissioningCompleteRsp
		response.Fields = fakeCommissioningStatus
	case command.Path.Endpoint == 1 && command.Path.Cluster == onOffCluster &&
		(command.Path.Command == 0 || command.Path.Command == 1):
		d.on = command.Path.Command == 1
		event = "control"
		status := im.Status{Global: im.StatusSuccess}
		response.Status = &status
	case command.Path.Endpoint == 0 && command.Path.Cluster == commissioning.ClusterOperationalCredentials &&
		command.Path.Command == commissioning.CommandRemoveFabric:
		index, ok := command.Fields.Field(0)
		if !ok || index.Type != tlv.TypeUint || index.Uint != 7 {
			return "", errors.New("RemoveFabric has the wrong fabric index")
		}
		d.removed = true
		event = "remove"
		response.Path.Command = commissioning.CommandNOCResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.PutUint(tlv.Context(1), 7)
			writer.EndContainer()
		}
	default:
		return "", fmt.Errorf("unexpected operational command %#v", command.Path)
	}
	encoded, err := im.EncodeInvokeResponseMessage([]im.CommandResponse{response}, false, false)
	if err != nil {
		return "", err
	}
	return event, exchange.Send(ctx, im.OpcodeInvokeResponse, encoded)
}

func fakeCommissioningStatus(writer *tlv.Writer, tag tlv.Tag) {
	writer.StartStructure(tag)
	writer.PutUint(tlv.Context(0), 0)
	writer.PutString(tlv.Context(1), "")
	writer.EndContainer()
}

func fakeBytes(value im.Value, number uint8, length int) ([]byte, error) {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeBytes || (length >= 0 && len(field.Data) != length) {
		return nil, fmt.Errorf("field %d is not a valid byte string", number)
	}
	return field.Data, nil
}

func fakeAttestationElements(nonce []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUint(tlv.Context(0), 1)
	writer.PutBytes(tlv.Context(2), nonce)
	writer.EndContainer()
	return writer.Bytes()
}

func fakeSign(key *ecdsa.PrivateKey, elements, challenge []byte) ([]byte, error) {
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

// commissionAndStore koppelt en bewaart wat eruit kwam.
//
// Commission levert kandidaten; bewaren is de keuze van wie het vroeg. In een
// test is dat altijd ja, net als vanaf de opdrachtregel -- er is geen scherm om
// op te kiezen.
func commissionAndStore(ctx context.Context, t *testing.T, controller *Controller,
	database *fakeBacking, request CommissionRequest) ([]Device, error) {
	t.Helper()
	candidates, err := controller.Commission(ctx, request)
	if err != nil {
		return nil, err
	}
	added := make([]Device, 0, len(candidates))
	for _, candidate := range candidates {
		device, addErr := database.AddDevice(ctx, candidate)
		if addErr != nil {
			return nil, addErr
		}
		added = append(added, device)
	}
	return added, nil
}
