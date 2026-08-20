// Package transport carries Matter messages over UDP and implements MRP,
// the Message Reliability Protocol: message counters, acknowledgements,
// retransmission with backoff, and duplicate detection. UDP gives none of
// that, so this is what turns the message codec into a conversation.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mathrand "math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/message"
)

// Port is Matter's operational UDP port.
const Port = 5540

// MRP constants from the Matter specification. The idle interval applies
// until a peer tells us otherwise through its session parameters.
const (
	MaxTransmissions = 5
	IdleRetryBase    = 500 * time.Millisecond
	backoffBase      = 1.6
	backoffJitter    = 0.25
	backoffMargin    = 1.1
	backoffThreshold = 1

	// Matter reserves the node-ID space above this value. An unauthenticated
	// initiator must still identify its PASE/CASE conversation with a random
	// ID from the operational range; responders echo it as their destination.
	maxOperationalNodeID = uint64(0xFFFFFFEFFFFFFFFF)
)

// Node owns one UDP socket and multiplexes Matter exchanges over it. It is
// both sides at once: it can initiate exchanges and accept them.
type Node struct {
	conn   *net.UDPConn
	conn6  *net.UDPConn // v6-baan waar het platform geen dual-stack "udp" kent (tamago); nil = conn dekt beide
	logger *slog.Logger

	// RetryInterval is the MRP base retransmission interval. Matter lets a
	// peer advertise its own idle and active intervals in its session
	// parameters; until that negotiation exists, this is the default and
	// the one knob tests turn down.
	RetryInterval time.Duration

	counter        atomic.Uint32
	nextExchangeID atomic.Uint32

	mu        sync.Mutex
	exchanges map[exchangeKey]*Exchange
	sessions  map[uint16]*SecureSession // indexed by the ID peers put on inbound messages
	closed    bool

	accepted  chan *Exchange
	closeOnce sync.Once
	done      chan struct{}
}

// Listen binds a UDP socket. Pass ":0" to let the system choose a port, or
// an empty host with Port for the standard Matter port.
func Listen(address string, logger *slog.Logger) (*Node, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Op een host bindt "udp" dual-stack en is één socket genoeg; op een
	// HopOS-node zijn v4 en v6 gescheiden sockets (leannet, IPV6_V6ONLY-
	// semantiek). De seam staat in listen_host.go / listen_tamago.go —
	// Thread-apparaten bestaan alleen op IPv6, dus de v6-baan is de reden
	// dat deze plugin op een node werkt.
	conn, conn6, err := listenSockets(address, logger)
	if err != nil {
		return nil, err
	}
	node := &Node{
		conn: conn, conn6: conn6, logger: logger, RetryInterval: IdleRetryBase,
		exchanges: make(map[exchangeKey]*Exchange),
		sessions:  make(map[uint16]*SecureSession),
		accepted:  make(chan *Exchange, 8),
		done:      make(chan struct{}),
	}
	// The specification requires message counters to start at a random
	// value, so a restart cannot replay a previous session's counters.
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	node.counter.Store(counterSeed(seed))
	var exchangeSeed [2]byte
	if _, err := rand.Read(exchangeSeed[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	node.nextExchangeID.Store(uint32(binary.LittleEndian.Uint16(exchangeSeed[:])))

	go node.readLoop(node.conn)
	if node.conn6 != nil {
		go node.readLoop(node.conn6)
	}
	return node, nil
}

// LocalAddr reports the bound address.
func (n *Node) LocalAddr() *net.UDPAddr { return n.conn.LocalAddr().(*net.UDPAddr) }

// sendConn kiest de socket voor dit adres: v6 over de v6-baan als die er is.
func (n *Node) sendConn(remote *net.UDPAddr) *net.UDPConn {
	if n.conn6 != nil && remote != nil && remote.IP.To4() == nil {
		return n.conn6
	}
	return n.conn
}

// Close stops the node and fails every exchange still in flight.
func (n *Node) Close() error {
	var err error
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		exchanges := make([]*Exchange, 0, len(n.exchanges))
		for _, exchange := range n.exchanges {
			exchanges = append(exchanges, exchange)
		}
		n.exchanges = make(map[exchangeKey]*Exchange)
		for _, session := range n.sessions {
			session.clear()
		}
		n.sessions = make(map[uint16]*SecureSession)
		n.mu.Unlock()
		close(n.done)
		err = n.conn.Close()
		if n.conn6 != nil {
			_ = n.conn6.Close()
		}
		for _, exchange := range exchanges {
			exchange.fail(errors.New("transport closed"))
		}
	})
	return err
}

func (n *Node) nextCounter() uint32 { return n.counter.Add(1) }

// exchangeKey identifies one exchange. weStarted is part of the identity and
// not a detail: Matter numbers exchanges per initiator, so a peer counts from
// its own seed and may legitimately pick an ID we are already using on the
// same session. Key without it and the peer's message finds our exchange,
// fails the initiator test, and is dropped — which is exactly a subscription
// report going missing (measured 19-08: two of these in one log).
type exchangeKey struct {
	sessionID uint16
	remote    string
	id        uint16
	weStarted bool
}

func keyFor(remote *net.UDPAddr, sessionID, exchangeID uint16, weStarted bool) exchangeKey {
	return exchangeKey{sessionID: sessionID, remote: remote.String(), id: exchangeID, weStarted: weStarted}
}

// SessionConfig contains the state negotiated by PASE or CASE. LocalID is
// what the peer puts on packets sent to Stulp; PeerID is what Stulp puts on
// packets sent to the peer. The two directions deliberately use different
// keys.
type SessionConfig struct {
	LocalID, PeerID         uint16
	LocalNodeID, PeerNodeID uint64
	OutboundKey, InboundKey []byte
	Remote                  *net.UDPAddr

	// Timing is this peer's MRP retransmission timing. It belongs to the
	// session and not to one exchange: a sleepy Thread device is just as slow
	// to answer a read as it was to answer Sigma1.
	Timing MRPTiming
}

// MRPTiming is what a peer says about its own reachability: the three values it
// advertises as SII, SAI and SAT (DNS-SD TXT, or the session parameters in a
// handshake). Zero fields mean the node default, which is only right for a
// mains-powered peer.
//
// Both intervals exist because both are true at different moments. A sleepy
// device may take its full idle interval to notice a packet — seventeen seconds
// in this fabric — but once it has just spoken to us it is awake and answers in
// milliseconds. Retransmitting on the idle interval the whole time would make a
// single lost packet cost seventeen seconds on a light switch; retransmitting on
// the active interval the whole time is what declares a sleeping device dead.
type MRPTiming struct {
	Idle            time.Duration // SII: the peer may be asleep
	Active          time.Duration // SAI: it was awake a moment ago
	ActiveThreshold time.Duration // SAT: how long that moment lasts
}

// SecureSession owns encryption, counters and replay state for one peer.
// It is intentionally transport state rather than exchange state: many
// Interaction Model exchanges share one PASE or CASE session.
type SecureSession struct {
	LocalID, PeerID         uint16
	LocalNodeID, PeerNodeID uint64
	Remote                  *net.UDPAddr
	outboundKey             []byte
	inboundKey              []byte
	// timing is set once at registration and read by every exchange on this
	// session; nothing mutates it, so it needs no lock. lastHeard does move,
	// from the receive path, and picks the interval below.
	timing    MRPTiming
	lastHeard atomic.Int64
	counter   atomic.Uint32
	replayMu  sync.Mutex
	replay    replayWindow
}

// heard records authenticated inbound traffic from this peer.
func (s *SecureSession) heard(now time.Time) { s.lastHeard.Store(now.UnixNano()) }

// retransBase is the peer's retransmission base right now: the active interval
// while it is known to be awake, the idle interval once it may have gone back to
// sleep. Zero means the caller falls back to the node default.
func (s *SecureSession) retransBase(now time.Time) time.Duration {
	if s.timing.Active > 0 && s.timing.ActiveThreshold > 0 {
		if last := s.lastHeard.Load(); last != 0 && now.UnixNano()-last < int64(s.timing.ActiveThreshold) {
			return s.timing.Active
		}
	}
	if s.timing.Idle > 0 {
		return s.timing.Idle
	}
	return s.timing.Active
}

// RegisterSession installs a negotiated session and returns the live session
// handle used to initiate secured exchanges.
func (n *Node) RegisterSession(config SessionConfig) (*SecureSession, error) {
	if config.LocalID == 0 || config.PeerID == 0 {
		return nil, errors.New("secure session IDs must be non-zero")
	}
	if len(config.OutboundKey) != 16 || len(config.InboundKey) != 16 {
		return nil, errors.New("secure session keys must be 16 bytes per direction")
	}
	if config.Remote == nil {
		return nil, errors.New("secure session needs a remote address")
	}
	seed, err := randomCounterSeed()
	if err != nil {
		return nil, err
	}
	remote := *config.Remote
	remote.IP = append(net.IP(nil), config.Remote.IP...)
	session := &SecureSession{
		LocalID: config.LocalID, PeerID: config.PeerID,
		LocalNodeID: config.LocalNodeID, PeerNodeID: config.PeerNodeID,
		Remote:      &remote,
		outboundKey: append([]byte(nil), config.OutboundKey...),
		inboundKey:  append([]byte(nil), config.InboundKey...),
		timing:      config.Timing,
	}
	session.counter.Store(seed)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		session.clear()
		return nil, errors.New("transport closed")
	}
	if _, exists := n.sessions[config.LocalID]; exists {
		session.clear()
		return nil, fmt.Errorf("local session ID %d is already in use", config.LocalID)
	}
	n.sessions[config.LocalID] = session
	return session, nil
}

// RemoveSession expires a secure session and every exchange that belongs to
// it. Callers establish a new PASE/CASE session before continuing.
func (n *Node) RemoveSession(localID uint16) {
	n.mu.Lock()
	session := n.sessions[localID]
	delete(n.sessions, localID)
	var exchanges []*Exchange
	for key, exchange := range n.exchanges {
		if key.sessionID == localID {
			delete(n.exchanges, key)
			exchanges = append(exchanges, exchange)
		}
	}
	n.mu.Unlock()
	if session != nil {
		session.clear()
	}
	for _, exchange := range exchanges {
		exchange.fail(errors.New("secure session expired"))
	}
}

func (s *SecureSession) clear() {
	clear(s.outboundKey)
	clear(s.inboundKey)
}

func (s *SecureSession) nextCounter() uint32 { return s.counter.Add(1) }

func (s *SecureSession) markReceived(counter uint32) bool {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	return s.replay.mark(counter)
}

// Initiate starts a new exchange toward remote.
func (n *Node) Initiate(remote *net.UDPAddr, protocolID uint16) (*Exchange, error) {
	return n.initiate(remote, nil, protocolID, 0)
}

// InitiateWithRetry starts an unsecured exchange using the peer's advertised
// MRP idle/active interval as its retransmission base. CASE Sigma1 is sent
// before a secure session exists, so this exchange cannot learn that timing
// from a session header yet.
func (n *Node) InitiateWithRetry(remote *net.UDPAddr, protocolID uint16, retryBase time.Duration) (*Exchange, error) {
	if retryBase < 0 {
		return nil, errors.New("exchange retry interval cannot be negative")
	}
	return n.initiate(remote, nil, protocolID, retryBase)
}

// InitiateSecure starts an exchange over a registered PASE or CASE session.
// Its retransmission timing comes from the session, so a read to a sleepy peer
// gets the same patience its CASE handshake got — see Exchange.retryInterval.
func (n *Node) InitiateSecure(session *SecureSession, protocolID uint16) (*Exchange, error) {
	if session == nil {
		return nil, errors.New("secure exchange needs a session")
	}
	return n.initiate(session.Remote, session, protocolID, 0)
}

func (n *Node) initiate(remote *net.UDPAddr, session *SecureSession, protocolID uint16, retryBase time.Duration) (*Exchange, error) {
	if remote == nil {
		return nil, errors.New("exchange needs a remote address")
	}
	ephemeralInitiatorID := uint64(0)
	if session == nil {
		var err error
		ephemeralInitiatorID, err = randomOperationalNodeID()
		if err != nil {
			return nil, fmt.Errorf("allocate ephemeral initiator node ID: %w", err)
		}
	}
	id := uint16(n.nextExchangeID.Add(1))
	localSessionID := uint16(0)
	if session != nil {
		localSessionID = session.LocalID
	}
	key := keyFor(remote, localSessionID, id, true)
	exchange := newExchange(n, remote, key, protocolID, session, ephemeralInitiatorID)
	exchange.retryBase = retryBase
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("transport closed")
	}
	if session != nil && n.sessions[session.LocalID] != session {
		return nil, errors.New("secure session is not registered on this transport")
	}
	if _, taken := n.exchanges[key]; taken {
		return nil, fmt.Errorf("exchange ID %d is already in use", id)
	}
	n.exchanges[key] = exchange
	return exchange, nil
}

// Accept returns the next exchange a peer started. Its first message is
// already buffered, so Receive returns it immediately.
func (n *Node) Accept(ctx context.Context) (*Exchange, error) {
	select {
	case exchange := <-n.accepted:
		return exchange, nil
	case <-n.done:
		return nil, errors.New("transport closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *Node) readLoop(conn *net.UDPConn) {
	buffer := make([]byte, 2048)
	for {
		count, remote, err := conn.ReadFromUDP(buffer)
		if err != nil {
			select {
			case <-n.done:
			default:
				n.logger.Debug("Matter transport read failed", "error", err)
			}
			return
		}
		packet := make([]byte, count)
		copy(packet, buffer[:count])
		n.dispatch(packet, remote)
	}
}

func (n *Node) dispatch(packet []byte, remote *net.UDPAddr) {
	header, _, err := message.PeekHeader(packet)
	if err != nil {
		n.logger.Debug("Matter transport dropped an unparsable message", "remote", remote, "error", err)
		return
	}

	var msg message.Message
	var session *SecureSession
	duplicate := false
	if header.SessionID == 0 {
		msg, err = message.Parse(packet)
	} else {
		n.mu.Lock()
		session = n.sessions[header.SessionID]
		n.mu.Unlock()
		if session == nil || !sameUDPAddr(session.Remote, remote) {
			n.logger.Debug("Matter transport dropped a message for an unknown secure session",
				"session", header.SessionID, "remote", remote)
			return
		}
		msg, err = message.OpenWithSource(packet, session.inboundKey, session.PeerNodeID)
		if err == nil {
			// Authenticate first. Advancing a replay window from the cleartext
			// counter before the MIC verifies would let an attacker lock out a peer.
			duplicate = session.markReceived(msg.Header.Counter)
			// And it proves the peer is awake: unauthenticated traffic must never
			// be able to talk us into the short retransmission interval.
			session.heard(time.Now())
		}
	}
	if err != nil {
		n.logger.Debug("Matter transport dropped an unauthenticated message", "remote", remote, "error", err)
		return
	}
	if session == nil {
		hasSource := msg.Header.SourceNodeID != nil
		hasDestination := msg.Header.DestinationNodeID != nil
		if hasSource == hasDestination {
			n.logger.Debug("Matter transport dropped a malformed unsecured message: exactly one ephemeral node ID is required",
				"remote", remote)
			return
		}
	}
	localSessionID := uint16(0)
	if session != nil {
		localSessionID = session.LocalID
	}
	// The initiator flag says whose numbering this ID belongs to: a message with
	// it set starts (or continues) the peer's exchange, one without it answers
	// ours. Both can carry the same ID at the same time.
	key := keyFor(remote, localSessionID, msg.Protocol.ExchangeID, !msg.Protocol.Initiator)

	n.mu.Lock()
	exchange, known := n.exchanges[key]
	shouldAccept := !known && msg.Protocol.Initiator && !n.closed
	if shouldAccept {
		ephemeralInitiatorID := uint64(0)
		if session == nil {
			// An unsecured peer that starts an exchange identifies the
			// unauthenticated session by its source node ID. Every response
			// addresses that same temporary ID.
			shouldAccept = msg.Header.SourceNodeID != nil
			if shouldAccept {
				ephemeralInitiatorID = *msg.Header.SourceNodeID
				shouldAccept = ephemeralInitiatorID != 0 && ephemeralInitiatorID <= maxOperationalNodeID
			}
		}
		if shouldAccept {
			exchange = newExchange(n, remote, key, msg.Protocol.ProtocolID, session, ephemeralInitiatorID)
			n.exchanges[key] = exchange
		}
	}
	n.mu.Unlock()

	if exchange == nil {
		// Een betrouwbaar antwoord op een uitwisseling die wij al sloten is
		// vrijwel altijd een retransmit waarvan ons afsluitende ack verloren
		// ging — Acknowledge is één onbetrouwbaar schot. Stil droppen liet de
		// peer tot vijf keer opnieuw zenden (gemeten 20-08: 18 drops per
		// apparaat in één ringbuffer), en een strenge publisher mag dat zijn
		// abonnement aanrekenen. Dus: alsnog een standalone ack, zónder de
		// boodschap te bezorgen — dezelfde keuze als connectedhomeip's
		// ephemeral ack. Alleen op een beveiligde sessie: die bewijst wie het
		// vraagt, en een onbeveiligd zwerfpakket verdient geen antwoord.
		if session != nil && msg.Protocol.Reliable {
			n.ackWithoutExchange(session, remote, msg)
		}
		n.logger.Debug("Matter transport dropped a message for an unknown exchange",
			"exchange", msg.Protocol.ExchangeID, "remote", remote)
		return
	}
	if !exchange.carries(msg.Protocol) {
		n.logger.Debug("Matter transport dropped a message that does not belong to its exchange",
			"exchange", msg.Protocol.ExchangeID, "remote", remote,
			"protocol", msg.Protocol.ProtocolID, "opcode", msg.Protocol.Opcode)
		return
	}
	if session == nil {
		var addressedID uint64
		if exchange.initiator {
			if msg.Header.DestinationNodeID == nil {
				n.logger.Debug("Matter transport dropped an unsecured response without a destination node ID",
					"exchange", msg.Protocol.ExchangeID, "remote", remote)
				return
			}
			addressedID = *msg.Header.DestinationNodeID
		} else {
			if msg.Header.SourceNodeID == nil {
				n.logger.Debug("Matter transport dropped an unsecured request without a source node ID",
					"exchange", msg.Protocol.ExchangeID, "remote", remote)
				return
			}
			addressedID = *msg.Header.SourceNodeID
		}
		if addressedID != exchange.ephemeralInitiatorID {
			n.logger.Debug("Matter transport dropped an unsecured message for another ephemeral initiator",
				"exchange", msg.Protocol.ExchangeID, "remote", remote)
			return
		}
	}
	exchange.deliver(msg, duplicate)
	if shouldAccept {
		select {
		case n.accepted <- exchange:
		default:
			n.logger.Debug("Matter transport dropped an inbound exchange: backlog full")
			n.forget(key)
			exchange.fail(errors.New("inbound exchange backlog full"))
		}
	}
}

// ackWithoutExchange beantwoordt een betrouwbaar bericht voor een al gesloten
// uitwisseling met een standalone ack. De rolvlag spiegelt de afzender: wie
// óns antwoordt krijgt een ack van de initiator die wij op die uitwisseling
// waren. Best effort — als dit pakket ook verloren gaat, retransmit de peer en
// komt hij hier gewoon opnieuw langs.
func (n *Node) ackWithoutExchange(session *SecureSession, remote *net.UDPAddr, received message.Message) {
	counter := received.Header.Counter
	frame, err := message.Message{
		Header: message.Header{SessionID: session.PeerID, Counter: session.nextCounter()},
		Protocol: message.ProtocolHeader{
			Initiator:  !received.Protocol.Initiator,
			AckCounter: &counter,
			Opcode:     message.OpcodeStandaloneAck,
			ExchangeID: received.Protocol.ExchangeID,
			ProtocolID: message.ProtocolSecureChannel,
		},
	}.SealWithSource(session.outboundKey, session.LocalNodeID)
	if err != nil {
		return
	}
	_, _ = n.sendConn(remote).WriteToUDP(frame, remote)
}

func sameUDPAddr(left, right *net.UDPAddr) bool {
	return left != nil && right != nil && left.Port == right.Port && left.Zone == right.Zone && left.IP.Equal(right.IP)
}

func (n *Node) forget(key exchangeKey) {
	n.mu.Lock()
	delete(n.exchanges, key)
	n.mu.Unlock()
}

// Exchange is one conversation: a sequence of related messages sharing an
// exchange ID, made reliable by MRP.
type Exchange struct {
	node       *Node
	remote     *net.UDPAddr
	key        exchangeKey
	id         uint16
	protocolID uint16
	initiator  bool
	session    *SecureSession
	retryBase  time.Duration

	// Non-zero only on unsecured PASE/CASE exchanges. Matter uses this ID
	// together with the peer address to route unauthenticated replies before a
	// secure session ID exists.
	ephemeralInitiatorID uint64

	mu         sync.Mutex
	pendingAck *uint32
	awaiting   map[uint32]chan error
	received   replayWindow
	failure    error

	incoming chan message.Message
}

// newExchange takes who initiated from the key, so the routing identity and
// the exchange's own idea of its role can never disagree.
func newExchange(node *Node, remote *net.UDPAddr, key exchangeKey, protocolID uint16, session *SecureSession, ephemeralInitiatorID uint64) *Exchange {
	return &Exchange{
		node: node, remote: remote, key: key, id: key.id, protocolID: protocolID, initiator: key.weStarted,
		session: session, ephemeralInitiatorID: ephemeralInitiatorID, awaiting: make(map[uint32]chan error),
		incoming: make(chan message.Message, 4),
	}
}

// carries reports whether this message belongs to the exchange's protocol.
//
// It is deliberately not an equality test. An MRP standalone acknowledgement is
// always Secure Channel opcode 0x10, whatever protocol the exchange itself
// speaks, because MRP sits under the protocols rather than inside one. Equality
// therefore drops precisely the message a slow peer sends to say "alive, stop
// retransmitting" — measured 19-08 on the two devices in this fabric with a
// 17-second idle interval, whose reads then ran out all five transmissions.
func (e *Exchange) carries(header message.ProtocolHeader) bool {
	if e.protocolID == header.ProtocolID {
		return true
	}
	return header.ProtocolID == message.ProtocolSecureChannel && header.Opcode == message.OpcodeStandaloneAck
}

// ID reports the exchange ID.
func (e *Exchange) ID() uint16 { return e.id }

// ProtocolID identifies the protocol that owns a peer-initiated exchange.
func (e *Exchange) ProtocolID() uint16 { return e.protocolID }

// PeerNodeID identifies the CASE peer. PASE and unsecured exchanges return 0.
func (e *Exchange) PeerNodeID() uint64 {
	if e.session == nil {
		return 0
	}
	return e.session.PeerNodeID
}

// Remote reports the peer's address.
func (e *Exchange) Remote() *net.UDPAddr { return e.remote }

// Close releases the exchange.
func (e *Exchange) Close() {
	e.node.forget(e.key)
	e.fail(errors.New("exchange closed"))
}

// Send transmits a message reliably: it keeps retransmitting with backoff
// until the peer acknowledges, either standalone or on its reply.
func (e *Exchange) Send(ctx context.Context, opcode uint8, payload []byte) error {
	frame, counter, err := e.build(e.protocolID, opcode, payload, true)
	if err != nil {
		return err
	}
	acknowledged := make(chan error, 1)
	e.mu.Lock()
	if e.failure != nil {
		err = e.failure
	} else {
		e.awaiting[counter] = acknowledged
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		e.mu.Lock()
		delete(e.awaiting, counter)
		e.mu.Unlock()
	}()

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for attempt := range MaxTransmissions {
		if _, err := e.node.sendConn(e.remote).WriteToUDP(frame, e.remote); err != nil {
			return fmt.Errorf("send Matter message: %w", err)
		}
		timer.Reset(e.retryInterval(attempt))
		select {
		case ackErr := <-acknowledged:
			return ackErr
		case <-timer.C:
			e.node.logger.Debug("Matter message not acknowledged, retransmitting",
				"exchange", e.id, "counter", counter, "attempt", attempt+1)
		case <-ctx.Done():
			return ctx.Err()
		case <-e.node.done:
			return errors.New("transport closed")
		}
	}
	return fmt.Errorf("peer did not acknowledge message %d after %d transmissions", counter, MaxTransmissions)
}

// SendOnce transmits without requesting an acknowledgement. Status reports
// that end an exchange use this.
func (e *Exchange) SendOnce(opcode uint8, payload []byte) error {
	frame, _, err := e.build(e.protocolID, opcode, payload, false)
	if err != nil {
		return err
	}
	if _, err := e.node.sendConn(e.remote).WriteToUDP(frame, e.remote); err != nil {
		return fmt.Errorf("send Matter message: %w", err)
	}
	return nil
}

// Acknowledge flushes an outstanding acknowledgement as a standalone ack,
// for when there is no reply to piggyback it on.
//
// It goes out under Secure Channel even on an Interaction Model exchange: MRP
// is below the protocols, and a peer that matches on protocol plus opcode (as
// connectedhomeip does) would otherwise not see an acknowledgement at all but
// an unknown Interaction Model message type.
func (e *Exchange) Acknowledge() error {
	e.mu.Lock()
	pending := e.pendingAck
	e.mu.Unlock()
	if pending == nil {
		return nil
	}
	frame, _, err := e.build(message.ProtocolSecureChannel, message.OpcodeStandaloneAck, nil, false)
	if err != nil {
		return err
	}
	if _, err := e.node.sendConn(e.remote).WriteToUDP(frame, e.remote); err != nil {
		return fmt.Errorf("send Matter message: %w", err)
	}
	return nil
}

// Receive waits for the peer's next message in this exchange.
func (e *Exchange) Receive(ctx context.Context) (opcode uint8, payload []byte, err error) {
	select {
	case msg := <-e.incoming:
		return msg.Protocol.Opcode, msg.Payload, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-e.node.done:
		return 0, nil, errors.New("transport closed")
	}
}

func (e *Exchange) build(protocolID uint16, opcode uint8, payload []byte, reliable bool) ([]byte, uint32, error) {
	e.mu.Lock()
	ack := e.pendingAck
	e.pendingAck = nil
	failure := e.failure
	e.mu.Unlock()
	if failure != nil {
		return nil, 0, failure
	}
	counter := e.node.nextCounter()
	sessionID := uint16(0)
	if e.session != nil {
		counter = e.session.nextCounter()
		sessionID = e.session.PeerID
	}

	msg := message.Message{
		Header: message.Header{SessionID: sessionID, Counter: counter},
		Protocol: message.ProtocolHeader{
			Initiator:  e.initiator,
			Reliable:   reliable,
			AckCounter: ack,
			Opcode:     opcode,
			ExchangeID: e.id,
			ProtocolID: protocolID,
		},
		Payload: payload,
	}
	if e.session == nil {
		if e.initiator {
			msg.Header.SourceNodeID = message.Ptr(e.ephemeralInitiatorID)
		} else {
			msg.Header.DestinationNodeID = message.Ptr(e.ephemeralInitiatorID)
		}
	}
	var frame []byte
	var err error
	if e.session == nil {
		frame, err = msg.Encode()
	} else {
		frame, err = msg.SealWithSource(e.session.outboundKey, e.session.LocalNodeID)
	}
	if err != nil {
		return nil, 0, err
	}
	if len(frame) > message.MaxUDPPayload {
		return nil, 0, fmt.Errorf("message is %d bytes, over the %d-byte UDP budget", len(frame), message.MaxUDPPayload)
	}
	return frame, counter, nil
}

// deliver handles one inbound message: it releases whatever it acknowledges,
// drops duplicates, and queues real protocol messages for Receive.
func (e *Exchange) deliver(msg message.Message, duplicate bool) {
	e.mu.Lock()
	if ack := msg.Protocol.AckCounter; ack != nil {
		if waiter, ok := e.awaiting[*ack]; ok {
			delete(e.awaiting, *ack)
			waiter <- nil
		}
	}
	if e.session == nil {
		duplicate = e.received.mark(msg.Header.Counter)
	}
	if msg.Protocol.Reliable {
		counter := msg.Header.Counter
		e.pendingAck = &counter
	}
	e.mu.Unlock()

	// A standalone acknowledgement carries no protocol content.
	if msg.Protocol.ProtocolID == message.ProtocolSecureChannel &&
		msg.Protocol.Opcode == message.OpcodeStandaloneAck {
		return
	}
	if duplicate {
		// The peer retransmitted because our acknowledgement was lost.
		// Re-acknowledge, but do not hand the message up twice.
		e.node.logger.Debug("Matter transport saw a duplicate", "exchange", e.id, "counter", msg.Header.Counter)
		_ = e.Acknowledge()
		return
	}
	select {
	case e.incoming <- msg:
	default:
		e.node.logger.Debug("Matter transport dropped a message: exchange backlog full", "exchange", e.id)
	}
}

func (e *Exchange) fail(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failure == nil {
		e.failure = err
	}
	for counter, waiter := range e.awaiting {
		delete(e.awaiting, counter)
		waiter <- err
	}
}

// replayWindow is a bounded sliding window. Matter sessions expire before a
// 32-bit counter wraps, so ordinary unsigned ordering is sufficient.
type replayWindow struct {
	initialized bool
	maximum     uint32
	seen        uint64
}

// mark records counter and reports whether it has already been seen or is too
// old for the 64-message acceptance window.
func (w *replayWindow) mark(counter uint32) bool {
	if !w.initialized {
		w.initialized, w.maximum, w.seen = true, counter, 1
		return false
	}
	if counter > w.maximum {
		delta := counter - w.maximum
		if delta >= 64 {
			w.seen = 1
		} else {
			w.seen = w.seen<<delta | 1
		}
		w.maximum = counter
		return false
	}
	delta := w.maximum - counter
	if delta >= 64 {
		return true
	}
	bit := uint64(1) << delta
	if w.seen&bit != 0 {
		return true
	}
	w.seen |= bit
	return false
}

func randomCounterSeed() (uint32, error) {
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return 0, err
	}
	return counterSeed(seed), nil
}

func randomOperationalNodeID() (uint64, error) {
	for {
		var bytes [8]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return 0, err
		}
		id := binary.LittleEndian.Uint64(bytes[:])
		if id != 0 && id <= maxOperationalNodeID {
			return id, nil
		}
	}
}

func counterSeed(seed [4]byte) uint32 {
	return binary.LittleEndian.Uint32(seed[:]) & 0x0FFFFFFF
}

// retryInterval follows the specification's backoff: exponential after the
// first retry, with jitter so a room full of devices does not resend in
// lockstep.
func (n *Node) retryInterval(attempt int) time.Duration {
	return retryInterval(n.RetryInterval, attempt)
}

// retryInterval is the wait before transmission attempt+1. A secured exchange
// asks its session every time instead of copying a base at creation: whether the
// peer counts as awake changes while the exchange is open, and that is the whole
// point of the two intervals.
func (e *Exchange) retryInterval(attempt int) time.Duration {
	base := e.retryBase
	if base <= 0 && e.session != nil {
		base = e.session.retransBase(time.Now())
	}
	if base <= 0 {
		base = e.node.RetryInterval
	}
	return retryInterval(base, attempt)
}

func retryInterval(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = IdleRetryBase
	}
	exponent := max(0, attempt-backoffThreshold)
	interval := float64(base) * backoffMargin * math.Pow(backoffBase, float64(exponent))
	return time.Duration(interval * (1 + backoffJitter*mathrand.Float64()))
}
