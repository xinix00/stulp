package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/casesession"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

type Server struct {
	manager  *Manager
	node     *transport.Node
	identity *serverIdentity
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	pendingMu sync.Mutex
	pending   map[uint16]*pendingCommission

	subMu            sync.Mutex
	subscriptions    map[uint32]*subscription
	nextSubscription atomic.Uint32
}

type pendingCommission struct {
	challenge []byte
	root      []byte
	key       *ecdsa.PrivateKey
	csr       []byte
	fabric    uint8
}

func StartServer(parent context.Context, manager *Manager, logger *slog.Logger) (*Server, error) {
	if manager == nil {
		return nil, errors.New("Matter bridge server needs an endpoint manager")
	}
	if logger == nil {
		logger = slog.Default()
	}
	record := manager.Record().Server
	normalized, identity, err := ensureServerRecord(record)
	if err != nil {
		return nil, err
	}
	if err := manager.updateServer(normalized); err != nil {
		return nil, fmt.Errorf("save Matter bridge identity: %w", err)
	}
	node, err := transport.Listen(net.JoinHostPort("", fmt.Sprint(normalized.Port)), logger)
	if err != nil {
		return nil, fmt.Errorf("listen for Matter bridge on UDP %d: %w", normalized.Port, err)
	}
	ctx, cancel := context.WithCancel(parent)
	server := &Server{
		manager: manager, node: node, identity: identity, logger: logger, ctx: ctx, cancel: cancel,
		pending: map[uint16]*pendingCommission{}, subscriptions: map[uint32]*subscription{},
	}
	server.nextSubscription.Store(uint32(time.Now().UnixNano()))
	server.wg.Add(2)
	go func() { defer server.wg.Done(); server.acceptLoop() }()
	go func() { defer server.wg.Done(); server.reportLoop() }()
	return server, nil
}

func (s *Server) Close() error {
	s.cancel()
	err := s.node.Close()
	s.wg.Wait()
	return err
}

func (s *Server) Address() string { return s.node.LocalAddr().String() }

func (s *Server) PairingPayload() (onboarding.Payload, error) {
	record := s.manager.Record().Server
	return onboarding.Payload{
		Version: 0, VendorID: BridgeVendorID, ProductID: BridgeProductID,
		Discovery: 1 << 2, Discriminator: record.Discriminator, Passcode: record.Passcode,
	}, nil
}

func (s *Server) acceptLoop() {
	for {
		exchange, err := s.node.Accept(s.ctx)
		if err != nil {
			if s.ctx.Err() == nil {
				s.logger.Warn("Matter bridge accept failed", "error", err)
			}
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.handleExchange(exchange); err != nil && s.ctx.Err() == nil {
				s.logger.Debug("Matter bridge exchange failed", "remote", exchange.Remote(), "error", err)
			}
		}()
	}
}

func (s *Server) handleExchange(exchange *transport.Exchange) error {
	opcode, payload, err := exchange.Receive(s.ctx)
	if err != nil {
		exchange.Close()
		return err
	}
	if exchange.ProtocolID() == message.ProtocolSecureChannel {
		switch opcode {
		case message.OpcodePBKDFParamRequest:
			return s.acceptPASE(exchange, payload)
		case message.OpcodeCASESigma1:
			return s.acceptCASE(exchange, payload)
		default:
			exchange.Close()
			return fmt.Errorf("unsupported Secure Channel opcode 0x%02x", opcode)
		}
	}
	if exchange.ProtocolID() != message.ProtocolInteractionModel {
		exchange.Close()
		return fmt.Errorf("unsupported Matter protocol 0x%04x", exchange.ProtocolID())
	}
	defer exchange.Close()
	if opcode == im.OpcodeTimedRequest {
		status, encodeErr := im.EncodeStatusResponse(im.StatusSuccess)
		if encodeErr != nil {
			return encodeErr
		}
		if err := exchange.Send(s.ctx, im.OpcodeStatusResponse, status); err != nil {
			return err
		}
		opcode, payload, err = exchange.Receive(s.ctx)
		if err != nil {
			return err
		}
	}
	switch opcode {
	case im.OpcodeReadRequest:
		return s.respondRead(exchange, payload)
	case im.OpcodeSubscribeRequest:
		return s.respondSubscribe(exchange, payload)
	case im.OpcodeInvokeRequest:
		return s.respondInvoke(exchange, payload)
	case im.OpcodeWriteRequest:
		return s.respondWrite(exchange, payload)
	default:
		return fmt.Errorf("unsupported Interaction Model opcode 0x%02x", opcode)
	}
}

func (s *Server) acceptPASE(exchange *transport.Exchange, request []byte) error {
	remote := exchange.Remote()
	session, err := s.identity.pase.ServeRequest(s.ctx, exchange, request)
	exchange.Close()
	if err != nil {
		return err
	}
	secure, err := s.node.RegisterSession(transport.SessionConfig{
		LocalID: session.LocalSessionID, PeerID: session.PeerSessionID,
		OutboundKey: session.Keys.R2I, InboundKey: session.Keys.I2R, Remote: remote,
	})
	if err != nil {
		return err
	}
	s.pendingMu.Lock()
	s.pending[secure.LocalID] = &pendingCommission{challenge: append([]byte(nil), session.Keys.AttestationChallenge...)}
	s.pendingMu.Unlock()
	return nil
}

func (s *Server) acceptCASE(exchange *transport.Exchange, sigma1 []byte) error {
	configs, err := s.responderConfigs()
	if err != nil {
		exchange.Close()
		return err
	}
	_, err = casesession.AcceptSigma1Any(s.ctx, s.node, exchange, sigma1, configs)
	return err
}

func (s *Server) responderConfigs() ([]casesession.ResponderConfig, error) {
	fabrics := s.manager.Record().Server.Fabrics
	configs := make([]casesession.ResponderConfig, 0, len(fabrics))
	for _, fabric := range fabrics {
		key, err := parseOperationalPrivateKey(fabric.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("fabric %d private key: %w", fabric.Index, err)
		}
		root, err := casesession.ParseCertificatePublicKey(fabric.Root)
		if err != nil {
			return nil, fmt.Errorf("fabric %d root: %w", fabric.Index, err)
		}
		configs = append(configs, casesession.ResponderConfig{
			FabricIndex: fabric.Index, FabricID: fabric.FabricID, AdminNodeID: fabric.AdminNodeID,
			IPK: fabric.IPK, RootPublicKey: root, LocalNodeID: fabric.NodeID,
			PrivateKey: key, NOC: fabric.NOC, ICAC: fabric.ICAC,
		})
	}
	return configs, nil
}

func parseOperationalPrivateKey(encoded []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(encoded)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("operational key is not ECDSA")
	}
	return key, nil
}
