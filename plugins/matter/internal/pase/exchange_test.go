package pase

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const testPasscode = 20202021

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pair starts a device listening on loopback and a commissioner node, both
// with a short MRP interval so retransmission tests stay fast. Traffic can be
// routed through a relay to simulate loss.
func pair(t *testing.T, relay func(t *testing.T, target *net.UDPAddr) *net.UDPAddr) (
	commissioner *transport.Node, deviceAddr *net.UDPAddr, sessions <-chan result) {
	t.Helper()

	parameters, err := DefaultParameters()
	if err != nil {
		t.Fatal(err)
	}
	device, err := NewDevice(testPasscode, parameters)
	if err != nil {
		t.Fatal(err)
	}
	deviceNode, err := transport.Listen("127.0.0.1:0", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	deviceNode.RetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { deviceNode.Close() })

	outcome := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange, err := deviceNode.Accept(ctx)
		if err != nil {
			outcome <- result{err: err}
			return
		}
		defer exchange.Close()
		session, err := device.Serve(ctx, exchange)
		outcome <- result{session: session, err: err}
	}()

	commissioner, err = transport.Listen("127.0.0.1:0", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	commissioner.RetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { commissioner.Close() })

	target := deviceNode.LocalAddr()
	if relay != nil {
		target = relay(t, target)
	}
	return commissioner, target, outcome
}

type result struct {
	session *Session
	err     error
}

// The whole stack over real sockets: UDP, MRP, the message frame, the PASE
// messages, SPAKE2+ and AES-CCM.
func TestCommissioningOverUDP(t *testing.T) {
	commissioner, deviceAddr, deviceOutcome := pair(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exchange, err := commissioner.Initiate(deviceAddr, message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	commissionerSession, err := Commission(ctx, exchange, testPasscode)
	if err != nil {
		t.Fatalf("commissioning failed: %v", err)
	}
	outcome := <-deviceOutcome
	if outcome.err != nil {
		t.Fatalf("the device side failed: %v", outcome.err)
	}
	deviceSession := outcome.session

	if !bytes.Equal(commissionerSession.Keys.I2R, deviceSession.Keys.I2R) ||
		!bytes.Equal(commissionerSession.Keys.R2I, deviceSession.Keys.R2I) ||
		!bytes.Equal(commissionerSession.Keys.AttestationChallenge, deviceSession.Keys.AttestationChallenge) {
		t.Fatal("the two sides derived different session keys")
	}
	// Each side tells the other which ID to address it by, so the two must
	// be mirrored.
	if commissionerSession.LocalSessionID != deviceSession.PeerSessionID {
		t.Fatalf("session IDs are not mirrored: commissioner local %d, device peer %d",
			commissionerSession.LocalSessionID, deviceSession.PeerSessionID)
	}
	if commissionerSession.PeerSessionID != deviceSession.LocalSessionID {
		t.Fatalf("session IDs are not mirrored: commissioner peer %d, device local %d",
			commissionerSession.PeerSessionID, deviceSession.LocalSessionID)
	}
	if commissionerSession.LocalSessionID == 0 || commissionerSession.PeerSessionID == 0 {
		t.Fatal("session ID 0 means unsecured and must not be allocated")
	}

	// The session must actually protect traffic.
	frame, err := message.Message{
		Header: message.Header{SessionID: commissionerSession.PeerSessionID, Counter: 1},
		Protocol: message.ProtocolHeader{
			Initiator: true, Opcode: 0x02, ProtocolID: message.ProtocolInteractionModel,
		},
		Payload: []byte("read attribute"),
	}.Seal(commissionerSession.Keys.I2R)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := message.Open(frame, deviceSession.Keys.I2R)
	if err != nil {
		t.Fatalf("the device could not open a frame on the new session: %v", err)
	}
	if !bytes.Equal(opened.Payload, []byte("read attribute")) {
		t.Fatalf("payload came through as %q", opened.Payload)
	}
	if _, err := message.Open(frame, deviceSession.Keys.R2I); err == nil {
		t.Fatal("the reverse-direction key must not open a forward-direction frame")
	}
}

func TestProbeStopsAfterPBKDFRoundTrip(t *testing.T) {
	commissioner, deviceAddr, _ := pair(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exchange, err := commissioner.Initiate(deviceAddr, message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()
	result, err := Probe(ctx, exchange)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if result.LocalSessionID == 0 || result.Response.ResponderSessionID == 0 {
		t.Fatalf("probe returned unsecured session IDs: %+v", result)
	}
	if result.Response.Parameters == nil || result.Response.Parameters.Iterations == 0 {
		t.Fatalf("probe lost the device PBKDF parameters: %+v", result.Response)
	}
}

// MRP has to survive a lossy network: every dropped packet must be
// retransmitted, and the duplicates that causes must not confuse either side.
func TestCommissioningSurvivesPacketLoss(t *testing.T) {
	var forwarded int
	dropped := map[int]bool{1: true, 4: true, 5: true}
	commissioner, deviceAddr, deviceOutcome := pair(t, func(t *testing.T, target *net.UDPAddr) *net.UDPAddr {
		return startRelay(t, target, func() bool {
			forwarded++
			return dropped[forwarded]
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exchange, err := commissioner.Initiate(deviceAddr, message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	commissionerSession, err := Commission(ctx, exchange, testPasscode)
	if err != nil {
		t.Fatalf("commissioning did not survive packet loss: %v", err)
	}
	outcome := <-deviceOutcome
	if outcome.err != nil {
		t.Fatalf("the device side failed: %v", outcome.err)
	}
	if !bytes.Equal(commissionerSession.Keys.I2R, outcome.session.Keys.I2R) {
		t.Fatal("the two sides derived different session keys")
	}
}

func TestWrongPasscodeIsRejectedOverTheWire(t *testing.T) {
	commissioner, deviceAddr, deviceOutcome := pair(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exchange, err := commissioner.Initiate(deviceAddr, message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	if _, err := Commission(ctx, exchange, testPasscode+1); err == nil {
		t.Fatal("a wrong passcode established a session")
	}
	outcome := <-deviceOutcome
	if outcome.err == nil {
		t.Fatal("the device accepted a commissioner with the wrong passcode")
	}
	if outcome.session != nil {
		t.Fatal("the device produced a session despite the failure")
	}
}

// A commissioner that never answers must not leave the device hanging: MRP
// gives up after MaxTransmissions.
func TestUnansweredExchangeGivesUp(t *testing.T) {
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	node, err := transport.Listen("127.0.0.1:0", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	node.RetryInterval = 10 * time.Millisecond
	defer node.Close()

	exchange, err := node.Initiate(silent.LocalAddr().(*net.UDPAddr), message.ProtocolSecureChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Commission(ctx, exchange, testPasscode); err == nil {
		t.Fatal("commissioning succeeded against a silent peer")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("giving up took %v; MRP should stop after %d transmissions",
			time.Since(started), transport.MaxTransmissions)
	}
}

// startRelay forwards UDP between one commissioner and the device, dropping
// packets whenever drop reports true. It returns the address the
// commissioner should target.
func startRelay(t *testing.T, target *net.UDPAddr, drop func() bool) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buffer := make([]byte, 2048)
		var client *net.UDPAddr
		for {
			count, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if drop() {
				continue
			}
			destination := target
			if from.Port == target.Port && from.IP.Equal(target.IP) {
				if client == nil {
					continue
				}
				destination = client
			} else {
				client = from
			}
			if _, err := conn.WriteToUDP(buffer[:count], destination); err != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}
