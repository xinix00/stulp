package pase

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/xinix00/stulp/plugins/matter/internal/crypto"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

// Session is what a completed PASE exchange produces.
type Session struct {
	Keys *crypto.SessionKeys
	// LocalSessionID is the ID the peer puts on messages to us.
	LocalSessionID uint16
	// PeerSessionID is the ID we put on messages to the peer.
	PeerSessionID uint16
}

// ProbeResult is the non-secret result of the first PASE round trip. Probe
// deliberately stops before the passcode is used, so it can distinguish
// transport/wire-format failures from authentication failures without
// installing or changing a fabric on the device.
type ProbeResult struct {
	LocalSessionID uint16
	Response       PBKDFParamResponse
}

// DefaultParameters are the PBKDF parameters a device offers when it has no
// reason to pick others. The iteration count is the specification's minimum:
// enough to slow an offline guess, cheap enough for small hardware.
func DefaultParameters() (PBKDFParameters, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return PBKDFParameters{}, err
	}
	return PBKDFParameters{Iterations: 1000, Salt: salt}, nil
}

// Commission runs the commissioner side of PASE: it turns a passcode into an
// encrypted session with the device on the other end of the exchange.
//
// The exchange must be freshly initiated on the Secure Channel protocol and
// must be unsecured — PASE is how a session key comes to exist, so it cannot
// use one.
func Commission(ctx context.Context, exchange *transport.Exchange, passcode uint32) (*Session, error) {
	requestBytes, responseBytes, localSessionID, response, err := exchangeParameters(ctx, exchange)
	if err != nil {
		return nil, err
	}

	scalars, err := crypto.DeriveScalars(passcode, response.Parameters.Salt, int(response.Parameters.Iterations))
	if err != nil {
		return nil, err
	}
	// The transcript is bound to the exact bytes both sides exchanged.
	prover, err := crypto.NewProver(crypto.Context(requestBytes, responseBytes), scalars)
	if err != nil {
		return nil, err
	}

	pake1, err := Pake1{PA: prover.Share()}.Encode()
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodePASEPake1, pake1); err != nil {
		return nil, fmt.Errorf("send Pake1: %w", err)
	}

	pake2Bytes, err := expect(ctx, exchange, message.OpcodePASEPake2)
	if err != nil {
		return nil, err
	}
	pake2, err := DecodePake2(pake2Bytes)
	if err != nil {
		return nil, err
	}
	confirmation, keys, err := prover.Finish(pake2.PB, pake2.CB)
	if err != nil {
		// Report the rejection so the device can stop waiting, then fail.
		_ = exchange.SendOnce(message.OpcodeStatusReport, Failure(StatusInvalidParameter).Encode())
		return nil, err
	}

	pake3, err := Pake3{CA: confirmation}.Encode()
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodePASEPake3, pake3); err != nil {
		return nil, fmt.Errorf("send Pake3: %w", err)
	}

	reportBytes, err := expect(ctx, exchange, message.OpcodeStatusReport)
	if err != nil {
		return nil, err
	}
	// Nothing follows, so the final acknowledgement has to stand alone.
	// Send it before judging the report, so a rejecting device also stops
	// retransmitting.
	_ = exchange.Acknowledge()
	report, err := DecodeStatusReport(reportBytes)
	if err != nil {
		return nil, err
	}
	if !report.OK() {
		return nil, fmt.Errorf("device rejected the session: %w", report)
	}

	return &Session{
		Keys:           keys,
		LocalSessionID: localSessionID,
		PeerSessionID:  response.ResponderSessionID,
	}, nil
}

// Probe performs only PBKDFParamRequest/Response. No passcode is transmitted
// or evaluated and no fabric state is touched. The final standalone ack keeps
// the peer from retransmitting its response while its temporary PASE exchange
// expires normally.
func Probe(ctx context.Context, exchange *transport.Exchange) (*ProbeResult, error) {
	_, _, localSessionID, response, err := exchangeParameters(ctx, exchange)
	if err != nil {
		return nil, err
	}
	if err := exchange.Acknowledge(); err != nil {
		return nil, fmt.Errorf("acknowledge PBKDFParamResponse: %w", err)
	}
	return &ProbeResult{LocalSessionID: localSessionID, Response: response}, nil
}

func exchangeParameters(ctx context.Context, exchange *transport.Exchange) (
	requestBytes, responseBytes []byte, localSessionID uint16, response PBKDFParamResponse, err error,
) {
	initiatorRandom := make([]byte, RandomSize)
	if _, err = rand.Read(initiatorRandom); err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, err
	}
	localSessionID, err = randomSessionID()
	if err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, err
	}

	request := PBKDFParamRequest{
		InitiatorRandom:    initiatorRandom,
		InitiatorSessionID: localSessionID,
		PasscodeID:         0,
		HasPBKDFParameters: false,
	}
	requestBytes, err = request.Encode()
	if err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, err
	}
	if err := exchange.Send(ctx, message.OpcodePBKDFParamRequest, requestBytes); err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, fmt.Errorf("send PBKDFParamRequest: %w", err)
	}

	responseBytes, err = expect(ctx, exchange, message.OpcodePBKDFParamResponse)
	if err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, err
	}
	response, err = DecodePBKDFParamResponse(responseBytes)
	if err != nil {
		return nil, nil, 0, PBKDFParamResponse{}, err
	}
	if !equalBytes(response.InitiatorRandom, initiatorRandom) {
		// The device echoes our random back; a mismatch means this is not a
		// reply to our request.
		return nil, nil, 0, PBKDFParamResponse{}, errors.New("PBKDFParamResponse does not echo our initiator random")
	}
	if response.Parameters == nil {
		return nil, nil, 0, PBKDFParamResponse{}, errors.New("device sent no PBKDF parameters and we did not claim to have them")
	}
	return requestBytes, responseBytes, localSessionID, response, nil
}

// Device is the commissionable side: what a device needs to answer a
// commissioner without ever storing the passcode.
type Device struct {
	Registration crypto.Registration
	Parameters   PBKDFParameters
}

// NewDevice derives a device's PASE configuration from a passcode. A real
// device would do this once at manufacturing time and keep only the result.
func NewDevice(passcode uint32, parameters PBKDFParameters) (*Device, error) {
	scalars, err := crypto.DeriveScalars(passcode, parameters.Salt, int(parameters.Iterations))
	if err != nil {
		return nil, err
	}
	return &Device{Registration: scalars.Register(), Parameters: parameters}, nil
}

// Serve runs the device side of PASE on an accepted exchange.
func (d *Device) Serve(ctx context.Context, exchange *transport.Exchange) (*Session, error) {
	requestBytes, err := expect(ctx, exchange, message.OpcodePBKDFParamRequest)
	if err != nil {
		return nil, err
	}
	request, err := DecodePBKDFParamRequest(requestBytes)
	if err != nil {
		return nil, d.reject(exchange, StatusInvalidParameter, err)
	}
	if request.PasscodeID != 0 {
		return nil, d.reject(exchange, StatusInvalidParameter,
			fmt.Errorf("passcode ID %d is not the commissioning passcode", request.PasscodeID))
	}

	responderRandom := make([]byte, RandomSize)
	if _, err := rand.Read(responderRandom); err != nil {
		return nil, err
	}
	localSessionID, err := randomSessionID()
	if err != nil {
		return nil, err
	}
	response := PBKDFParamResponse{
		InitiatorRandom:    request.InitiatorRandom,
		ResponderRandom:    responderRandom,
		ResponderSessionID: localSessionID,
	}
	if !request.HasPBKDFParameters {
		parameters := d.Parameters
		response.Parameters = &parameters
	}
	responseBytes, err := response.Encode()
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodePBKDFParamResponse, responseBytes); err != nil {
		return nil, fmt.Errorf("send PBKDFParamResponse: %w", err)
	}

	verifier, err := crypto.NewVerifier(crypto.Context(requestBytes, responseBytes), d.Registration)
	if err != nil {
		return nil, err
	}

	pake1Bytes, err := expect(ctx, exchange, message.OpcodePASEPake1)
	if err != nil {
		return nil, err
	}
	pake1, err := DecodePake1(pake1Bytes)
	if err != nil {
		return nil, d.reject(exchange, StatusInvalidParameter, err)
	}
	share, confirmation, err := verifier.Accept(pake1.PA)
	if err != nil {
		return nil, d.reject(exchange, StatusInvalidParameter, err)
	}
	pake2, err := Pake2{PB: share, CB: confirmation}.Encode()
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, message.OpcodePASEPake2, pake2); err != nil {
		return nil, fmt.Errorf("send Pake2: %w", err)
	}

	pake3Bytes, err := expect(ctx, exchange, message.OpcodePASEPake3)
	if err != nil {
		return nil, err
	}
	pake3, err := DecodePake3(pake3Bytes)
	if err != nil {
		return nil, d.reject(exchange, StatusInvalidParameter, err)
	}
	keys, err := verifier.Confirm(pake3.CA)
	if err != nil {
		return nil, d.reject(exchange, StatusInvalidParameter, err)
	}

	// Sent reliably: if this were lost the commissioner would wait forever
	// for a session the device already considers established.
	if err := exchange.Send(ctx, message.OpcodeStatusReport, SessionEstablished().Encode()); err != nil {
		return nil, fmt.Errorf("send status report: %w", err)
	}
	return &Session{
		Keys:           keys,
		LocalSessionID: localSessionID,
		PeerSessionID:  request.InitiatorSessionID,
	}, nil
}

// reject tells the commissioner why the exchange failed, then returns the
// original error. A device that stays silent leaves the commissioner
// retransmitting until it times out.
func (d *Device) reject(exchange *transport.Exchange, code uint16, cause error) error {
	_ = exchange.SendOnce(message.OpcodeStatusReport, Failure(code).Encode())
	return cause
}

// expect receives the next message and insists on an opcode, turning a
// status report from the peer into a descriptive error.
func expect(ctx context.Context, exchange *transport.Exchange, want uint8) ([]byte, error) {
	opcode, payload, err := exchange.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if opcode == message.OpcodeStatusReport && want != message.OpcodeStatusReport {
		report, decodeErr := DecodeStatusReport(payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("peer aborted PASE with an unreadable status report: %w", decodeErr)
		}
		return nil, fmt.Errorf("peer aborted PASE: %w", report)
	}
	if opcode != want {
		return nil, fmt.Errorf("expected opcode 0x%02x, got 0x%02x", want, opcode)
	}
	return payload, nil
}

// randomSessionID picks a non-zero session ID; zero means "unsecured".
func randomSessionID() (uint16, error) {
	for {
		var value [2]byte
		if _, err := rand.Read(value[:]); err != nil {
			return 0, err
		}
		if id := binary.LittleEndian.Uint16(value[:]); id != 0 {
			return id, nil
		}
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
