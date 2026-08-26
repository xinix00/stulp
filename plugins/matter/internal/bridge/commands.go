package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"

	"github.com/xinix00/stulp/plugins/matter/internal/casesession"
	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const (
	clusterOnOff          uint32 = 0x0006
	clusterLevelControl   uint32 = 0x0008
	clusterWindowCovering uint32 = 0x0102
)

func (s *Server) respondInvoke(exchange *transport.Exchange, payload []byte) error {
	commands, err := im.DecodeInvokeRequest(payload)
	if err != nil {
		return err
	}
	responses := make([]im.CommandResponse, 0, len(commands))
	for _, command := range commands {
		response, commandErr := s.commandResponse(exchange, command)
		if commandErr != nil {
			status := im.Status{Global: im.StatusFailure}
			response = im.CommandResponse{Path: command.Path, Status: &status}
			s.logger.Warn("Matter bridge command failed", "endpoint", command.Path.Endpoint,
				"cluster", fmt.Sprintf("0x%04X", command.Path.Cluster), "command", command.Path.Command, "error", commandErr)
		}
		responses = append(responses, response)
	}
	encoded, err := im.EncodeInvokeResponseMessage(responses, false, false)
	if err != nil {
		return err
	}
	return exchange.Send(s.ctx, im.OpcodeInvokeResponse, encoded)
}

func (s *Server) commandResponse(exchange *transport.Exchange, command im.InvokeRequestCommand) (im.CommandResponse, error) {
	response := im.CommandResponse{Path: command.Path}
	success := func() im.CommandResponse {
		status := im.Status{Global: im.StatusSuccess}
		response.Status = &status
		return response
	}
	if command.Path.Endpoint == 0 {
		return s.commissioningCommand(exchange, command)
	}
	switch command.Path.Cluster {
	case clusterOnOff:
		switch command.Path.Command {
		case 0:
			return success(), s.manager.Invoke(command.Path.Endpoint, "off", nil)
		case 1:
			return success(), s.manager.Invoke(command.Path.Endpoint, "on", nil)
		case 2:
			device, _, ok := s.manager.Device(command.Path.Endpoint)
			if !ok {
				return response, errors.New("endpoint no longer exists")
			}
			on, _ := device.State["onoff"].(bool)
			if on {
				return success(), s.manager.Invoke(command.Path.Endpoint, "off", nil)
			}
			return success(), s.manager.Invoke(command.Path.Endpoint, "on", nil)
		}
	case clusterLevelControl:
		if command.Path.Command == 0x04 {
			level, ok := command.Fields.Field(0)
			if !ok || level.Type != tlv.TypeUint || level.Uint > 254 {
				return response, errors.New("MoveToLevelWithOnOff has no valid level")
			}
			return success(), s.manager.Invoke(command.Path.Endpoint, "level", float64(level.Uint)/254)
		}
	case clusterWindowCovering:
		switch command.Path.Command {
		case 0:
			return success(), s.manager.Invoke(command.Path.Endpoint, "open", nil)
		case 1:
			return success(), s.manager.Invoke(command.Path.Endpoint, "close", nil)
		case 2:
			return success(), s.manager.Invoke(command.Path.Endpoint, "stop", nil)
		case 5:
			position, ok := command.Fields.Field(0)
			if !ok || position.Type != tlv.TypeUint || position.Uint > 10000 {
				return response, errors.New("GoToLiftPercentage has no valid percentage")
			}
			return success(), s.manager.Invoke(command.Path.Endpoint, "closed_fraction", float64(position.Uint)/10000)
		}
	}
	status := im.Status{Global: im.StatusUnsupportedCommand}
	response.Status = &status
	return response, nil
}

func (s *Server) commissioningCommand(exchange *transport.Exchange, command im.InvokeRequestCommand) (im.CommandResponse, error) {
	response := im.CommandResponse{Path: command.Path}
	pending := s.pendingFor(exchange.SessionID())
	switch {
	case command.Path.Cluster == commissioning.ClusterGeneralCommissioning && command.Path.Command == commissioning.CommandArmFailSafe:
		response.Path.Command = commissioning.CommandArmFailSafeResponse
		response.Fields = commissioningStatus
	case command.Path.Cluster == commissioning.ClusterGeneralCommissioning && command.Path.Command == commissioning.CommandSetRegulatoryConfig:
		response.Path.Command = commissioning.CommandSetRegulatoryConfigRsp
		response.Fields = commissioningStatus
	case command.Path.Cluster == commissioning.ClusterGeneralCommissioning && command.Path.Command == commissioning.CommandCommissioningComplete:
		if pending == nil || pending.fabric == 0 {
			return response, errors.New("CommissioningComplete arrived before AddNOC")
		}
		response.Path.Command = commissioning.CommandCommissioningCompleteRsp
		response.Fields = commissioningStatus
		s.pendingMu.Lock()
		delete(s.pending, exchange.SessionID())
		s.pendingMu.Unlock()
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandCertificateChainRequest:
		if pending == nil {
			return response, errors.New("certificate requested outside PASE commissioning")
		}
		kind, ok := command.Fields.Field(0)
		if !ok || kind.Type != tlv.TypeUint {
			return response, errors.New("CertificateChainRequest has no type")
		}
		certificate := s.identity.dac.Raw
		if kind.Uint == uint64(commissioning.CertificateTypePAI) {
			certificate = s.identity.pai.Raw
		} else if kind.Uint != uint64(commissioning.CertificateTypeDAC) {
			return response, errors.New("unsupported certificate type")
		}
		response.Path.Command = commissioning.CommandCertificateChainResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutBytes(tlv.Context(0), certificate)
			writer.EndContainer()
		}
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandAttestationRequest:
		if pending == nil {
			return response, errors.New("attestation requested outside PASE commissioning")
		}
		nonce, err := commandBytes(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		elements, err := attestationElements(nonce)
		if err != nil {
			return response, err
		}
		signature, err := signAttestation(s.identity.key, elements, pending.challenge)
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
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandCSRRequest:
		if pending == nil {
			return response, errors.New("CSR requested outside PASE commissioning")
		}
		nonce, err := commandBytes(command.Fields, 0, 32)
		if err != nil {
			return response, err
		}
		pending.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return response, err
		}
		pending.csr, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, pending.key)
		if err != nil {
			return response, err
		}
		var writer tlv.Writer
		writer.StartStructure(tlv.Anonymous())
		writer.PutBytes(tlv.Context(1), pending.csr)
		writer.PutBytes(tlv.Context(2), nonce)
		writer.EndContainer()
		elements, err := writer.Bytes()
		if err != nil {
			return response, err
		}
		signature, err := signAttestation(s.identity.key, elements, pending.challenge)
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
		if pending == nil {
			return response, errors.New("root added outside PASE commissioning")
		}
		root, err := commandBytes(command.Fields, 0, -1)
		if err != nil {
			return response, err
		}
		if _, err := casesession.ParseCertificatePublicKey(root); err != nil {
			return response, fmt.Errorf("invalid Matter root certificate: %w", err)
		}
		pending.root = append([]byte(nil), root...)
		status := im.Status{Global: im.StatusSuccess}
		response.Status = &status
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandAddNOC:
		if pending == nil || pending.key == nil || len(pending.root) == 0 {
			return response, errors.New("AddNOC arrived before CSR/root")
		}
		fabric, err := s.addFabric(command.Fields, pending)
		if err != nil {
			return response, err
		}
		pending.fabric = fabric.Index
		response.Path.Command = commissioning.CommandNOCResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.PutUint(tlv.Context(1), uint64(fabric.Index))
			writer.EndContainer()
		}
	case command.Path.Cluster == commissioning.ClusterOperationalCredentials && command.Path.Command == commissioning.CommandRemoveFabric:
		index, ok := command.Fields.Field(0)
		if !ok || index.Type != tlv.TypeUint || index.Uint == 0 || index.Uint > 254 {
			return response, errors.New("RemoveFabric has no valid index")
		}
		if exchange.FabricIndex() != uint8(index.Uint) {
			return response, errors.New("a fabric may only remove itself from this bridge")
		}
		if err := s.removeFabric(uint8(index.Uint)); err != nil {
			return response, err
		}
		response.Path.Command = commissioning.CommandNOCResponse
		response.Fields = func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.PutUint(tlv.Context(1), index.Uint)
			writer.EndContainer()
		}
	default:
		status := im.Status{Global: im.StatusUnsupportedCommand}
		response.Status = &status
	}
	return response, nil
}

func (s *Server) pendingFor(sessionID uint16) *pendingCommission {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return s.pending[sessionID]
}

func (s *Server) addFabric(fields im.Value, pending *pendingCommission) (FabricRecord, error) {
	noc, err := commandBytes(fields, 0, -1)
	if err != nil {
		return FabricRecord{}, err
	}
	icac := []byte(nil)
	if value, ok := fields.Field(1); ok && value.Type == tlv.TypeBytes {
		icac = append([]byte(nil), value.Data...)
	}
	ipk, err := commandBytes(fields, 2, 16)
	if err != nil {
		return FabricRecord{}, err
	}
	admin, err := commandUint(fields, 3)
	if err != nil || admin == 0 {
		return FabricRecord{}, errors.New("AddNOC has no admin subject")
	}
	vendor, err := commandUint(fields, 4)
	if err != nil || vendor > 0xFFFF {
		return FabricRecord{}, errors.New("AddNOC has no admin vendor")
	}
	publicKey, nodeID, fabricID, err := casesession.ParseOperationalIdentity(noc)
	if err != nil {
		return FabricRecord{}, err
	}
	if !publicKey.Equal(&pending.key.PublicKey) {
		return FabricRecord{}, errors.New("AddNOC public key does not match the bridge CSR")
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(pending.key)
	if err != nil {
		return FabricRecord{}, err
	}
	record := s.manager.Record().Server
	index := nextFabricIndex(record.Fabrics)
	if index == 0 {
		return FabricRecord{}, errors.New("Matter bridge fabric table is full")
	}
	fabric := FabricRecord{Index: index, FabricID: fabricID, NodeID: nodeID, AdminNodeID: admin,
		AdminVendorID: uint16(vendor), IPK: append([]byte(nil), ipk...), Root: append([]byte(nil), pending.root...),
		NOC: append([]byte(nil), noc...), ICAC: icac, PrivateKey: privateKey}
	record.Fabrics = append(record.Fabrics, fabric)
	if err := s.manager.updateServer(record); err != nil {
		return FabricRecord{}, err
	}
	return fabric, nil
}

func (s *Server) removeFabric(index uint8) error {
	record := s.manager.Record().Server
	found := false
	kept := record.Fabrics[:0]
	for _, fabric := range record.Fabrics {
		if fabric.Index == index {
			found = true
			continue
		}
		kept = append(kept, fabric)
	}
	if !found {
		return errors.New("Matter bridge fabric does not exist")
	}
	record.Fabrics = kept
	return s.manager.updateServer(record)
}

func nextFabricIndex(fabrics []FabricRecord) uint8 {
	for candidate := 1; candidate < 255; candidate++ {
		found := false
		for _, fabric := range fabrics {
			if fabric.Index == uint8(candidate) {
				found = true
				break
			}
		}
		if !found {
			return uint8(candidate)
		}
	}
	return 0
}

func commissioningStatus(writer *tlv.Writer, tag tlv.Tag) {
	writer.StartStructure(tag)
	writer.PutUint(tlv.Context(0), 0)
	writer.PutString(tlv.Context(1), "")
	writer.EndContainer()
}

func commandBytes(value im.Value, number uint8, length int) ([]byte, error) {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeBytes || (length >= 0 && len(field.Data) != length) {
		return nil, fmt.Errorf("field %d is not a valid byte string", number)
	}
	return field.Data, nil
}
func commandUint(value im.Value, number uint8) (uint64, error) {
	field, ok := value.Field(number)
	if !ok || field.Type != tlv.TypeUint {
		return 0, fmt.Errorf("field %d is not an unsigned integer", number)
	}
	return field.Uint, nil
}

func attestationElements(nonce []byte) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUint(tlv.Context(0), 1)
	writer.PutBytes(tlv.Context(2), nonce)
	writer.EndContainer()
	return writer.Bytes()
}
func signAttestation(key *ecdsa.PrivateKey, elements, challenge []byte) ([]byte, error) {
	hash := sha256.Sum256(append(append([]byte(nil), elements...), challenge...))
	r, ss, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return nil, err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	return signature, nil
}

var _ = context.Background
var _ = big.NewInt
