package transport

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/message"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testNode starts a node plus a raw UDP socket standing in for a peer, so
// tests control the exact bytes on the wire.
func testNode(t *testing.T) (*Node, *net.UDPConn) {
	t.Helper()
	node, err := Listen("127.0.0.1:0", quiet())
	if err != nil {
		t.Fatal(err)
	}
	node.RetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { node.Close() })

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	return node, peer
}

func peerFrame(t *testing.T, counter uint32, exchangeID uint16, opcode uint8, reliable bool, payload []byte) []byte {
	t.Helper()
	frame, err := message.Message{
		Header: message.Header{Counter: counter, SourceNodeID: message.Ptr(uint64(0x1122334455667788))},
		Protocol: message.ProtocolHeader{
			Initiator: true, Reliable: reliable,
			Opcode: opcode, ExchangeID: exchangeID, ProtocolID: message.ProtocolSecureChannel,
		},
		Payload: payload,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func readFrom(t *testing.T, conn *net.UDPConn, within time.Duration) (message.Message, bool) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	count, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		return message.Message{}, false
	}
	msg, err := message.Parse(buffer[:count])
	if err != nil {
		t.Fatalf("node sent an unparsable frame: %v", err)
	}
	return msg, true
}

func TestAcceptBuffersTheFirstMessage(t *testing.T) {
	node, peer := testNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame := peerFrame(t, 1, 0x0777, message.OpcodePBKDFParamRequest, true, []byte{0x15, 0x18})
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	exchange, err := node.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exchange.ID() != 0x0777 {
		t.Fatalf("exchange ID %d, want 0x0777", exchange.ID())
	}
	opcode, payload, err := exchange.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opcode != message.OpcodePBKDFParamRequest || len(payload) != 2 {
		t.Fatalf("first message came through as opcode %#x, payload %X", opcode, payload)
	}
}

// A retransmission the peer sent because our acknowledgement was lost must be
// re-acknowledged but never handed up a second time.
func TestDuplicateIsReacknowledgedButNotRedelivered(t *testing.T) {
	node, peer := testNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame := peerFrame(t, 42, 0x0123, message.OpcodePBKDFParamRequest, true, []byte{0x15, 0x18})
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	exchange, err := node.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := exchange.Receive(ctx); err != nil {
		t.Fatal(err)
	}

	// The same frame again: same counter, so it is a duplicate.
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	acknowledgement, ok := readFrom(t, peer, 2*time.Second)
	if !ok {
		t.Fatal("the duplicate was not re-acknowledged")
	}
	if acknowledgement.Protocol.Opcode != message.OpcodeStandaloneAck {
		t.Fatalf("expected a standalone acknowledgement, got opcode %#x", acknowledgement.Protocol.Opcode)
	}
	if acknowledgement.Protocol.AckCounter == nil || *acknowledgement.Protocol.AckCounter != 42 {
		t.Fatalf("acknowledgement covers %v, want counter 42", acknowledgement.Protocol.AckCounter)
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()
	if _, _, err := exchange.Receive(shortCtx); err == nil {
		t.Fatal("the duplicate was delivered to the application twice")
	}
}

func TestMessageForAnUnknownExchangeIsDropped(t *testing.T) {
	node, peer := testNode(t)
	// Initiator flag clear: this is a reply to an exchange we never started.
	frame, err := message.Message{
		Header: message.Header{Counter: 1},
		Protocol: message.ProtocolHeader{
			Opcode: message.OpcodePASEPake2, ExchangeID: 0xBEEF,
			ProtocolID: message.ProtocolSecureChannel,
		},
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := node.Accept(ctx); err == nil {
		t.Fatal("an unsolicited reply created an exchange")
	}
}

func TestGarbageIsDropped(t *testing.T) {
	node, peer := testNode(t)
	for _, packet := range [][]byte{{}, {0x00}, {0xFF, 0xFF, 0xFF, 0xFF}, make([]byte, 12)} {
		if _, err := peer.WriteToUDP(packet, node.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := node.Accept(ctx); err == nil {
		t.Fatal("garbage created an exchange")
	}
}

func TestCountersStartRandomAndIncrement(t *testing.T) {
	first, peer := testNode(t)
	second, err := Listen("127.0.0.1:0", quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.counter.Load() == second.counter.Load() {
		t.Fatal("two nodes started from the same message counter; it must be random")
	}

	exchange, err := first.Initiate(peer.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	var counters []uint32
	for range 3 {
		if err := exchange.SendOnce(message.OpcodePBKDFParamRequest, nil); err != nil {
			t.Fatal(err)
		}
		msg, ok := readFrom(t, peer, 2*time.Second)
		if !ok {
			t.Fatal("no frame arrived")
		}
		counters = append(counters, msg.Header.Counter)
		if msg.Header.SourceNodeID == nil || *msg.Header.SourceNodeID != exchange.ephemeralInitiatorID {
			t.Fatalf("unsecured request carried source node ID %v, want %#x", msg.Header.SourceNodeID, exchange.ephemeralInitiatorID)
		}
		if msg.Header.DestinationNodeID != nil {
			t.Fatalf("unsecured initiator also carried a destination node ID: %v", msg.Header.DestinationNodeID)
		}
	}
	for index := 1; index < len(counters); index++ {
		if counters[index] != counters[index-1]+1 {
			t.Fatalf("counters did not increment by one: %v", counters)
		}
	}
}

func TestUnsecuredResponderEchoesEphemeralInitiatorAsDestination(t *testing.T) {
	node, peer := testNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const ephemeralID = uint64(0x1122334455667788)
	frame := peerFrame(t, 1, 0x0777, message.OpcodePBKDFParamRequest, true, nil)
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	exchange, err := node.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()
	if _, _, err := exchange.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := exchange.SendOnce(message.OpcodePBKDFParamResponse, nil); err != nil {
		t.Fatal(err)
	}
	response, ok := readFrom(t, peer, 2*time.Second)
	if !ok {
		t.Fatal("no response arrived")
	}
	if response.Header.DestinationNodeID == nil || *response.Header.DestinationNodeID != ephemeralID {
		t.Fatalf("response destination = %v, want %#x", response.Header.DestinationNodeID, ephemeralID)
	}
	if response.Header.SourceNodeID != nil {
		t.Fatalf("unsecured responder also carried a source node ID: %v", response.Header.SourceNodeID)
	}
}

func TestMalformedUnsecuredMessageWithoutEphemeralNodeIDIsDropped(t *testing.T) {
	node, peer := testNode(t)
	frame, err := message.Message{
		Header: message.Header{Counter: 1},
		Protocol: message.ProtocolHeader{
			Initiator: true, Opcode: message.OpcodePBKDFParamRequest,
			ExchangeID: 0x0777, ProtocolID: message.ProtocolSecureChannel,
		},
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := node.Accept(ctx); err == nil {
		t.Fatal("unsecured message without an ephemeral node ID created an exchange")
	}
}

func TestOversizedPayloadIsRejected(t *testing.T) {
	node, peer := testNode(t)
	exchange, err := node.Initiate(peer.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()
	if err := exchange.SendOnce(message.OpcodePBKDFParamRequest, make([]byte, message.MaxUDPPayload)); err == nil {
		t.Fatal("a payload over the UDP budget was sent")
	}
}

func TestSendGivesUpAfterMaxTransmissions(t *testing.T) {
	node, peer := testNode(t)
	exchange, err := node.Initiate(peer.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exchange.Send(ctx, message.OpcodePBKDFParamRequest, nil); err == nil {
		t.Fatal("Send succeeded without an acknowledgement")
	}

	// The peer should have seen exactly MaxTransmissions copies.
	seen := 0
	for {
		if _, ok := readFrom(t, peer, 200*time.Millisecond); !ok {
			break
		}
		seen++
	}
	if seen != MaxTransmissions {
		t.Fatalf("peer saw %d transmissions, want %d", seen, MaxTransmissions)
	}
}

func TestCloseUnblocksWaiters(t *testing.T) {
	node, peer := testNode(t)
	exchange, err := node.Initiate(peer.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 2)
	go func() {
		_, _, err := exchange.Receive(context.Background())
		failed <- err
	}()
	go func() {
		_, err := node.Accept(context.Background())
		failed <- err
	}()
	time.Sleep(50 * time.Millisecond)
	node.Close()

	for range 2 {
		select {
		case err := <-failed:
			if err == nil {
				t.Fatal("a waiter returned success after Close")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not unblock a waiter")
		}
	}
}

func TestSecureSessionCarriesBothDirections(t *testing.T) {
	initiator, err := Listen("127.0.0.1:0", quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer initiator.Close()
	responder, err := Listen("127.0.0.1:0", quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	i2r := bytes.Repeat([]byte{0x11}, 16)
	r2i := bytes.Repeat([]byte{0x22}, 16)
	initiatorSession, err := initiator.RegisterSession(SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: i2r, InboundKey: r2i,
		Remote: responder.LocalAddr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := responder.RegisterSession(SessionConfig{
		LocalID: 0x2001, PeerID: 0x1001, OutboundKey: r2i, InboundKey: i2r,
		Remote: initiator.LocalAddr(),
	}); err != nil {
		t.Fatal(err)
	}

	exchange, err := initiator.InitiateSecure(initiatorSession, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()
	if err := exchange.SendOnce(0x02, []byte("read")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peerExchange, err := responder.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer peerExchange.Close()
	opcode, payload, err := peerExchange.Receive(ctx)
	if err != nil || opcode != 0x02 || !bytes.Equal(payload, []byte("read")) {
		t.Fatalf("secured request = opcode %#x payload %q error %v", opcode, payload, err)
	}
	if err := peerExchange.SendOnce(0x05, []byte("report")); err != nil {
		t.Fatal(err)
	}
	opcode, payload, err = exchange.Receive(ctx)
	if err != nil || opcode != 0x05 || !bytes.Equal(payload, []byte("report")) {
		t.Fatalf("secured response = opcode %#x payload %q error %v", opcode, payload, err)
	}
}

func TestExchangeIDsAreScopedBySessionAndPeer(t *testing.T) {
	controller, err := Listen("127.0.0.1:0", quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	peerA, err := net.ResolveUDPAddr("udp", "127.0.0.1:41001")
	if err != nil {
		t.Fatal(err)
	}
	peerB, err := net.ResolveUDPAddr("udp", "127.0.0.1:41002")
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x33}, 16)
	sessionA, err := controller.RegisterSession(SessionConfig{
		LocalID: 1, PeerID: 11, OutboundKey: key, InboundKey: key, Remote: peerA,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := controller.RegisterSession(SessionConfig{
		LocalID: 2, PeerID: 22, OutboundKey: key, InboundKey: key, Remote: peerB,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.nextExchangeID.Store(99)
	first, err := controller.InitiateSecure(sessionA, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	controller.nextExchangeID.Store(99)
	second, err := controller.InitiateSecure(sessionB, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatalf("same exchange ID on another session was rejected: %v", err)
	}
	defer second.Close()
	if first.ID() != second.ID() {
		t.Fatalf("test did not create an intentional collision: %d != %d", first.ID(), second.ID())
	}
}

func TestReplayWindowIsBounded(t *testing.T) {
	var window replayWindow
	if window.mark(100) || window.mark(102) || window.mark(101) {
		t.Fatal("fresh and in-window out-of-order counters must be accepted")
	}
	if !window.mark(101) {
		t.Fatal("a duplicate counter was accepted")
	}
	if window.mark(200) {
		t.Fatal("a new counter after a large jump was rejected")
	}
	if !window.mark(100) {
		t.Fatal("a counter older than the replay window was accepted")
	}
}

func TestCloseFailsSendInsteadOfAcknowledgingIt(t *testing.T) {
	node, peer := testNode(t)
	exchange, err := node.Initiate(peer.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- exchange.Send(context.Background(), message.OpcodePBKDFParamRequest, nil) }()
	if _, ok := readFrom(t, peer, 2*time.Second); !ok {
		t.Fatal("the send never reached the peer")
	}
	_ = node.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closing the transport was mistaken for an acknowledgement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send stayed blocked after Close")
	}
}

// Backoff must grow, stay bounded by the jitter ceiling, and scale with the
// configured base interval.
func TestRetryIntervalBackoff(t *testing.T) {
	node := &Node{RetryInterval: 100 * time.Millisecond}
	var previous time.Duration
	for attempt := range MaxTransmissions {
		interval := node.retryInterval(attempt)
		lower := time.Duration(float64(node.RetryInterval) * backoffMargin)
		if attempt > backoffThreshold {
			lower = time.Duration(float64(lower) * pow(backoffBase, attempt-backoffThreshold))
		}
		upper := time.Duration(float64(lower) * (1 + backoffJitter))
		if interval < lower || interval > upper {
			t.Fatalf("attempt %d gave %v, want within [%v, %v]", attempt, interval, lower, upper)
		}
		if attempt > backoffThreshold && interval <= previous {
			t.Fatalf("attempt %d did not back off: %v after %v", attempt, interval, previous)
		}
		previous = interval
	}
	// An unset interval must fall back to the specification default rather
	// than firing continuously.
	if (&Node{}).retryInterval(0) < IdleRetryBase {
		t.Fatal("a zero RetryInterval did not fall back to the idle default")
	}
}

func TestExchangeRetryIntervalOverridesNodeDefault(t *testing.T) {
	exchange := &Exchange{node: &Node{RetryInterval: 100 * time.Millisecond}, retryBase: 15_800 * time.Millisecond}
	interval := exchange.retryInterval(0)
	lower := time.Duration(float64(exchange.retryBase) * backoffMargin)
	upper := time.Duration(float64(lower) * (1 + backoffJitter))
	if interval < lower || interval > upper {
		t.Fatalf("exchange retry interval = %v, want advertised base within [%v, %v]", interval, lower, upper)
	}
}

// A standalone acknowledgement is Secure Channel opcode 0x10 whatever protocol
// the exchange speaks. It is how a peer that cannot answer yet says "alive,
// stop retransmitting", so an Interaction Model exchange must accept it: the
// devices that need it most are the sleepy ones, and hammering them five times
// is the opposite of what they asked for.
func TestStandaloneAckReleasesAnInteractionExchange(t *testing.T) {
	node, peer := testNode(t)
	i2r := bytes.Repeat([]byte{0x44}, 16)
	r2i := bytes.Repeat([]byte{0x55}, 16)
	session, err := node.RegisterSession(SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: i2r, InboundKey: r2i,
		Remote: peer.LocalAddr().(*net.UDPAddr),
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := node.InitiateSecure(session, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() { sent <- exchange.Send(ctx, 0x02, []byte("subscribe")) }()

	// Read the request off the wire to learn the counter the peer must
	// acknowledge; the peer holds the other direction's key.
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	count, _, err := peer.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request, err := message.OpenWithSource(buffer[:count], i2r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if request.Protocol.ProtocolID != message.ProtocolInteractionModel {
		t.Fatalf("request went out on protocol %#x, want Interaction Model", request.Protocol.ProtocolID)
	}

	ack := request.Header.Counter
	frame, err := message.Message{
		Header: message.Header{SessionID: 0x1001, Counter: 7},
		Protocol: message.ProtocolHeader{
			AckCounter: &ack, Opcode: message.OpcodeStandaloneAck,
			ExchangeID: exchange.ID(), ProtocolID: message.ProtocolSecureChannel,
		},
	}.SealWithSource(r2i, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("standalone acknowledgement did not satisfy Send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the standalone acknowledgement was dropped: Send kept retransmitting")
	}
}

// Matter numbers exchanges per initiator, so the same ID can be live in both
// directions at once. A report from the device must not be swallowed by our own
// exchange that happens to carry that number.
func TestPeerMayStartAnExchangeWithAnIDWeAreUsing(t *testing.T) {
	node, peer := testNode(t)
	remote := peer.LocalAddr().(*net.UDPAddr)
	ours, err := node.Initiate(remote, message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close()

	frame := peerFrame(t, 7, ours.ID(), message.OpcodePBKDFParamRequest, true, []byte{0x15, 0x18})
	if _, err := peer.WriteToUDP(frame, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	accepted, err := node.Accept(ctx)
	if err != nil {
		t.Fatalf("the peer's exchange %d was dropped because we hold that ID ourselves: %v", ours.ID(), err)
	}
	defer accepted.Close()
	if accepted == ours {
		t.Fatal("the peer's message landed in our own exchange")
	}
	if _, _, err := accepted.Receive(ctx); err != nil {
		t.Fatal(err)
	}
}

// The patience CASE negotiated is the session's, not one exchange's: this
// fabric has devices advertising a 17-second idle interval, and the transport
// default of 500 ms gives up after 5.6 seconds in total.
func TestSecureExchangeInheritsTheSessionRetryBase(t *testing.T) {
	node, peer := testNode(t)
	key := bytes.Repeat([]byte{0x66}, 16)
	sleepy, err := node.RegisterSession(SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: key, InboundKey: key,
		Remote: peer.LocalAddr().(*net.UDPAddr), Timing: MRPTiming{Idle: 17 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := node.InitiateSecure(sleepy, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	var patience time.Duration
	for attempt := range MaxTransmissions {
		patience += exchange.retryInterval(attempt)
	}
	if patience < 100*time.Second {
		t.Fatalf("total patience %v: a peer allowed 17s per answer needs more than five short waits", patience)
	}

	awake, err := node.RegisterSession(SessionConfig{
		LocalID: 0x1002, PeerID: 0x2002, OutboundKey: key, InboundKey: key,
		Remote: peer.LocalAddr().(*net.UDPAddr),
	})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := node.InitiateSecure(awake, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := plain.retryInterval(0); got > time.Second {
		t.Fatalf("a session without advertised timing must fall back to the node default, got %v", got)
	}
}

// The idle interval is for a peer that may be asleep. One we heard from a moment
// ago is not asleep, and making a light switch wait seventeen seconds for a
// single lost packet would be the cure being worse than the disease.
func TestAPeerWeJustHeardFromGetsTheActiveInterval(t *testing.T) {
	node, peer := testNode(t)
	key := bytes.Repeat([]byte{0x77}, 16)
	session, err := node.RegisterSession(SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: key, InboundKey: key,
		Remote: peer.LocalAddr().(*net.UDPAddr),
		Timing: MRPTiming{Idle: 17 * time.Second, Active: 2500 * time.Millisecond, ActiveThreshold: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := node.InitiateSecure(session, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	// Never heard from: assume it sleeps.
	if got := exchange.retryInterval(0); got < 17*time.Second {
		t.Fatalf("first interval %v, want the idle interval for a peer we have not heard from", got)
	}
	now := time.Now()
	session.heard(now)
	if got := exchange.retryInterval(0); got > 4*time.Second {
		t.Fatalf("interval %v right after hearing from the peer, want the 2.5s active interval", got)
	}
	// And once the active threshold has passed it counts as asleep again.
	session.lastHeard.Store(now.Add(-2 * time.Second).UnixNano())
	if got := exchange.retryInterval(0); got < 17*time.Second {
		t.Fatalf("interval %v after the active threshold lapsed, want the idle interval again", got)
	}
}

func pow(base float64, exponent int) float64 {
	result := 1.0
	for range exponent {
		result *= base
	}
	return result
}

// Een retransmit voor een uitwisseling die wij al sloten — ons afsluitende ack
// is één onbetrouwbaar schot en kan verloren gaan — hoort een standalone ack te
// krijgen, niet vijf keer stilte. Zonder ack telt een strenge publisher de
// mislukte bezorging tegen het abonnement.
func TestLateRetransmitForAClosedExchangeGetsAnAck(t *testing.T) {
	node, peer := testNode(t)
	i2r := bytes.Repeat([]byte{0x24}, 16)
	r2i := bytes.Repeat([]byte{0x42}, 16)
	if _, err := node.RegisterSession(SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: i2r, InboundKey: r2i,
		Remote: peer.LocalAddr().(*net.UDPAddr),
	}); err != nil {
		t.Fatal(err)
	}

	// Het apparaat her-zendt zijn laatste betrouwbare antwoord op een
	// uitwisseling die wij ooit initieerden (Initiator=false) en al vergaten.
	retransmit, err := message.Message{
		Header: message.Header{SessionID: 0x1001, Counter: 99},
		Protocol: message.ProtocolHeader{
			Reliable: true, Opcode: 0x05, ExchangeID: 0x4BC6,
			ProtocolID: message.ProtocolInteractionModel,
		},
	}.SealWithSource(r2i, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(retransmit, node.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	count, _, err := peer.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal("geen standalone ack voor de late retransmit: de peer blijft zo tot vijf keer zenden")
	}
	ack, err := message.OpenWithSource(buffer[:count], i2r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Protocol.ProtocolID != message.ProtocolSecureChannel ||
		ack.Protocol.Opcode != message.OpcodeStandaloneAck {
		t.Fatalf("verwachtte een standalone ack, kreeg protocol %#x opcode %#x",
			ack.Protocol.ProtocolID, ack.Protocol.Opcode)
	}
	if ack.Protocol.AckCounter == nil || *ack.Protocol.AckCounter != 99 {
		t.Fatalf("ack dekt %v, wilde counter 99", ack.Protocol.AckCounter)
	}
	if ack.Protocol.ExchangeID != 0x4BC6 || !ack.Protocol.Initiator {
		t.Fatalf("ack hoort op dezelfde uitwisseling en met de gespiegelde rol: exchange %#x initiator %t",
			ack.Protocol.ExchangeID, ack.Protocol.Initiator)
	}

	// En het bericht is niet bezorgd: er is geen uitwisseling om aan te nemen.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := node.Accept(ctx); err == nil {
		t.Fatal("de late retransmit is als nieuwe uitwisseling aangenomen")
	}
}
