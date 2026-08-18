// Package message implements the Matter message frame: the unencrypted
// message header, the protocol (exchange) header that travels inside the
// encrypted payload, and the AES-CCM binding between them.
//
// Every Matter exchange rides on this frame. Commissioning starts on the
// unsecured session (session ID 0, Encode/Parse) and moves to an encrypted
// session (Seal/Open) once PASE or CASE has established keys.
package message

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/xinix00/stulp/plugins/matter/internal/crypto"
)

// Session types carried in the security flags.
const (
	SessionUnicast uint8 = 0
	SessionGroup   uint8 = 1
)

// Protocol IDs assigned by the Matter specification.
const (
	ProtocolSecureChannel    uint16 = 0x0000
	ProtocolInteractionModel uint16 = 0x0001
	ProtocolBDX              uint16 = 0x0002
	ProtocolUserDirectedComm uint16 = 0x0003
)

// Secure Channel opcodes. The PASE set is what commissioning needs first.
const (
	OpcodeStandaloneAck      uint8 = 0x10
	OpcodePBKDFParamRequest  uint8 = 0x20
	OpcodePBKDFParamResponse uint8 = 0x21
	OpcodePASEPake1          uint8 = 0x22
	OpcodePASEPake2          uint8 = 0x23
	OpcodePASEPake3          uint8 = 0x24
	OpcodeCASESigma1         uint8 = 0x30
	OpcodeCASESigma2         uint8 = 0x31
	OpcodeCASESigma3         uint8 = 0x32
	OpcodeStatusReport       uint8 = 0x40
)

// Message flag bits.
const (
	flagSourceNodeID  = 0x04
	flagDestNodeID    = 0x01
	flagDestGroupID   = 0x02
	flagDestMask      = 0x03
	flagVersionMask   = 0xF0
	flagMessageReserv = 0x08
)

// Security flag bits.
const (
	securityPrivacy     = 0x80
	securityControl     = 0x40
	securityExtensions  = 0x20
	securityReserved    = 0x1C
	securitySessionType = 0x03
)

// Exchange flag bits.
const (
	exchangeInitiator  = 0x01
	exchangeAck        = 0x02
	exchangeReliable   = 0x04
	exchangeExtensions = 0x08
	exchangeVendor     = 0x10
	exchangeReserved   = 0xE0
)

// MaxUDPPayload is the Matter message budget over UDP: the IPv6 minimum MTU
// minus IPv6 and UDP headers. Larger payloads must go over TCP or be
// segmented by BDX.
const MaxUDPPayload = 1280 - 40 - 8

// Header is the unencrypted message header. It authenticates every encrypted
// message as additional data, so its bytes are covered by the MIC even
// though they travel in the clear.
type Header struct {
	SessionID   uint16
	SessionType uint8
	// Control marks a control message (message-counter synchronization).
	Control bool
	Counter uint32

	// Optional addressing. A message carries at most one destination.
	SourceNodeID       *uint64
	DestinationNodeID  *uint64
	DestinationGroupID *uint16

	// Extensions is the optional message-extensions blob (unused today,
	// preserved so foreign messages round-trip).
	Extensions []byte
}

// ProtocolHeader is the exchange header. On an encrypted session it lives
// inside the ciphertext, so it is invisible to anyone without the key.
type ProtocolHeader struct {
	// Initiator marks the message as sent by the exchange's initiator.
	Initiator bool
	// Reliable requests an acknowledgement (MRP).
	Reliable bool
	// AckCounter acknowledges a previously received message counter.
	AckCounter *uint32

	Opcode     uint8
	ExchangeID uint16
	ProtocolID uint16
	// VendorID scopes ProtocolID to a vendor-specific protocol.
	VendorID   *uint16
	Extensions []byte
}

// Message is one complete Matter frame.
type Message struct {
	Header   Header
	Protocol ProtocolHeader
	Payload  []byte
}

// Encode serializes an unsecured message. Commissioning's opening exchanges
// (PBKDFParamRequest/Response, PASE Pake1..3) travel this way, on session
// ID 0, because no key exists yet.
func (m Message) Encode() ([]byte, error) {
	if m.Header.SessionID != 0 {
		return nil, fmt.Errorf("unsecured messages use session ID 0, got %d", m.Header.SessionID)
	}
	if m.Header.SessionType != SessionUnicast {
		return nil, errors.New("unsecured messages must use a unicast session")
	}
	header, err := m.Header.encode()
	if err != nil {
		return nil, err
	}
	protocol, err := m.Protocol.encode()
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(header)+len(protocol)+len(m.Payload))
	frame = append(frame, header...)
	frame = append(frame, protocol...)
	return append(frame, m.Payload...), nil
}

// Parse decodes an unsecured message.
func Parse(wire []byte) (Message, error) {
	header, rest, err := decodeHeader(wire)
	if err != nil {
		return Message{}, err
	}
	if header.SessionID != 0 {
		return Message{}, fmt.Errorf("message claims session ID %d but carries no MIC", header.SessionID)
	}
	protocol, payload, err := decodeProtocolHeader(rest)
	if err != nil {
		return Message{}, err
	}
	return Message{Header: header, Protocol: protocol, Payload: payload}, nil
}

// PeekHeader decodes only the cleartext message header and returns its wire
// length. A transport uses this to select the secure-session context before
// the encrypted protocol header can be authenticated and opened.
func PeekHeader(wire []byte) (Header, int, error) {
	header, rest, err := decodeHeader(wire)
	if err != nil {
		return Header{}, 0, err
	}
	return header, len(wire) - len(rest), nil
}

// Seal serializes the message and encrypts everything from the protocol
// header onwards with the session key. The message header travels in the
// clear but is authenticated as additional data.
func (m Message) Seal(key []byte) ([]byte, error) {
	var sourceNodeID uint64
	if m.Header.SourceNodeID != nil {
		sourceNodeID = *m.Header.SourceNodeID
	}
	return m.SealWithSource(key, sourceNodeID)
}

// SealWithSource encrypts a message using the nonce source node ID supplied
// by the secure-session context. PASE uses the unspecified node ID (zero);
// CASE uses the local operational node ID even when it is omitted from the
// compact unicast message header.
func (m Message) SealWithSource(key []byte, sourceNodeID uint64) ([]byte, error) {
	if m.Header.SessionID == 0 {
		return nil, errors.New("secured messages need a non-zero session ID")
	}
	header, err := m.Header.encode()
	if err != nil {
		return nil, err
	}
	protocol, err := m.Protocol.encode()
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, 0, len(protocol)+len(m.Payload))
	plaintext = append(plaintext, protocol...)
	plaintext = append(plaintext, m.Payload...)

	nonce := m.Header.nonce(header, sourceNodeID)
	return aead.Seal(header, nonce[:], plaintext, header), nil
}

// Open authenticates and decrypts a message with the session key.
func Open(wire, key []byte) (Message, error) {
	header, _, err := decodeHeader(wire)
	if err != nil {
		return Message{}, err
	}
	var sourceNodeID uint64
	if header.SourceNodeID != nil {
		sourceNodeID = *header.SourceNodeID
	}
	return OpenWithSource(wire, key, sourceNodeID)
}

// OpenWithSource authenticates and decrypts a message using the peer's nonce
// source node ID from the secure-session context.
func OpenWithSource(wire, key []byte, sourceNodeID uint64) (Message, error) {
	header, encrypted, err := decodeHeader(wire)
	if err != nil {
		return Message{}, err
	}
	if header.SessionID == 0 {
		return Message{}, errors.New("secured messages need a non-zero session ID")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return Message{}, err
	}
	additional := wire[:len(wire)-len(encrypted)]
	nonce := header.nonce(additional, sourceNodeID)
	plaintext, err := aead.Open(nil, nonce[:], encrypted, additional)
	if err != nil {
		return Message{}, err
	}
	protocol, payload, err := decodeProtocolHeader(plaintext)
	if err != nil {
		return Message{}, err
	}
	return Message{Header: header, Protocol: protocol, Payload: payload}, nil
}

func newAEAD(key []byte) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("Matter session keys are 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return crypto.NewCCM(block, crypto.TagSize, crypto.NonceSize)
}

// nonce builds the AES-CCM nonce: security flags, message counter and
// source node ID. The security flags byte is taken from the encoded header
// so the nonce always matches the bytes on the wire.
func (h Header) nonce(encodedHeader []byte, sourceNodeID uint64) [crypto.NonceSize]byte {
	var value [crypto.NonceSize]byte
	value[0] = encodedHeader[3]
	binary.LittleEndian.PutUint32(value[1:5], h.Counter)
	binary.LittleEndian.PutUint64(value[5:13], sourceNodeID)
	return value
}

func (h Header) encode() ([]byte, error) {
	if h.DestinationNodeID != nil && h.DestinationGroupID != nil {
		return nil, errors.New("a message carries either a destination node ID or a destination group ID")
	}
	if h.SessionType > SessionGroup {
		return nil, fmt.Errorf("session type %d is reserved", h.SessionType)
	}
	if h.SessionType == SessionGroup && h.DestinationGroupID == nil {
		return nil, errors.New("a group session needs a destination group ID")
	}
	if len(h.Extensions) > 0xFFFF {
		return nil, errors.New("message extensions exceed 65535 bytes")
	}

	var flags byte
	if h.SourceNodeID != nil {
		flags |= flagSourceNodeID
	}
	switch {
	case h.DestinationNodeID != nil:
		flags |= flagDestNodeID
	case h.DestinationGroupID != nil:
		flags |= flagDestGroupID
	}
	security := h.SessionType
	if h.Control {
		security |= securityControl
	}
	if len(h.Extensions) > 0 {
		security |= securityExtensions
	}

	out := make([]byte, 0, 26+len(h.Extensions))
	out = append(out, flags)
	out = binary.LittleEndian.AppendUint16(out, h.SessionID)
	out = append(out, security)
	out = binary.LittleEndian.AppendUint32(out, h.Counter)
	if h.SourceNodeID != nil {
		out = binary.LittleEndian.AppendUint64(out, *h.SourceNodeID)
	}
	if h.DestinationNodeID != nil {
		out = binary.LittleEndian.AppendUint64(out, *h.DestinationNodeID)
	}
	if h.DestinationGroupID != nil {
		out = binary.LittleEndian.AppendUint16(out, *h.DestinationGroupID)
	}
	if len(h.Extensions) > 0 {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(h.Extensions)))
		out = append(out, h.Extensions...)
	}
	return out, nil
}

// decodeHeader returns the header plus everything after it.
func decodeHeader(wire []byte) (Header, []byte, error) {
	cursor := &reader{data: wire}
	flags, err := cursor.byteValue()
	if err != nil {
		return Header{}, nil, fmt.Errorf("message header: %w", err)
	}
	if version := flags & flagVersionMask; version != 0 {
		return Header{}, nil, fmt.Errorf("unsupported message format version %d", version>>4)
	}
	if flags&flagMessageReserv != 0 {
		return Header{}, nil, errors.New("message flags set a reserved bit")
	}
	if flags&flagDestMask == flagDestMask {
		return Header{}, nil, errors.New("message flags use the reserved destination size")
	}

	var header Header
	if header.SessionID, err = cursor.uint16(); err != nil {
		return Header{}, nil, fmt.Errorf("message header: %w", err)
	}
	security, err := cursor.byteValue()
	if err != nil {
		return Header{}, nil, fmt.Errorf("message header: %w", err)
	}
	if security&securityPrivacy != 0 {
		// Privacy obfuscates parts of the header with a separate key. It is
		// only used for group messages, which Stulp does not send or accept
		// yet; rejecting is safer than mis-parsing.
		return Header{}, nil, errors.New("privacy-enhanced messages are not supported")
	}
	if security&securityReserved != 0 {
		return Header{}, nil, errors.New("security flags set a reserved bit")
	}
	header.SessionType = security & securitySessionType
	if header.SessionType > SessionGroup {
		return Header{}, nil, fmt.Errorf("session type %d is reserved", header.SessionType)
	}
	header.Control = security&securityControl != 0
	if header.Counter, err = cursor.uint32(); err != nil {
		return Header{}, nil, fmt.Errorf("message header: %w", err)
	}
	if flags&flagSourceNodeID != 0 {
		value, err := cursor.uint64()
		if err != nil {
			return Header{}, nil, fmt.Errorf("source node ID: %w", err)
		}
		header.SourceNodeID = &value
	}
	switch flags & flagDestMask {
	case flagDestNodeID:
		value, err := cursor.uint64()
		if err != nil {
			return Header{}, nil, fmt.Errorf("destination node ID: %w", err)
		}
		header.DestinationNodeID = &value
	case flagDestGroupID:
		value, err := cursor.uint16()
		if err != nil {
			return Header{}, nil, fmt.Errorf("destination group ID: %w", err)
		}
		header.DestinationGroupID = &value
	}
	if security&securityExtensions != 0 {
		if header.Extensions, err = cursor.lengthPrefixed(); err != nil {
			return Header{}, nil, fmt.Errorf("message extensions: %w", err)
		}
	}
	return header, cursor.rest(), nil
}

func (p ProtocolHeader) encode() ([]byte, error) {
	if len(p.Extensions) > 0xFFFF {
		return nil, errors.New("secured extensions exceed 65535 bytes")
	}
	var flags byte
	if p.Initiator {
		flags |= exchangeInitiator
	}
	if p.AckCounter != nil {
		flags |= exchangeAck
	}
	if p.Reliable {
		flags |= exchangeReliable
	}
	if len(p.Extensions) > 0 {
		flags |= exchangeExtensions
	}
	if p.VendorID != nil {
		flags |= exchangeVendor
	}

	out := make([]byte, 0, 12+len(p.Extensions))
	out = append(out, flags, p.Opcode)
	out = binary.LittleEndian.AppendUint16(out, p.ExchangeID)
	if p.VendorID != nil {
		out = binary.LittleEndian.AppendUint16(out, *p.VendorID)
	}
	out = binary.LittleEndian.AppendUint16(out, p.ProtocolID)
	if p.AckCounter != nil {
		out = binary.LittleEndian.AppendUint32(out, *p.AckCounter)
	}
	if len(p.Extensions) > 0 {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(p.Extensions)))
		out = append(out, p.Extensions...)
	}
	return out, nil
}

func decodeProtocolHeader(data []byte) (ProtocolHeader, []byte, error) {
	cursor := &reader{data: data}
	flags, err := cursor.byteValue()
	if err != nil {
		return ProtocolHeader{}, nil, fmt.Errorf("protocol header: %w", err)
	}
	if flags&exchangeReserved != 0 {
		return ProtocolHeader{}, nil, errors.New("exchange flags set a reserved bit")
	}
	header := ProtocolHeader{
		Initiator: flags&exchangeInitiator != 0,
		Reliable:  flags&exchangeReliable != 0,
	}
	if header.Opcode, err = cursor.byteValue(); err != nil {
		return ProtocolHeader{}, nil, fmt.Errorf("protocol header: %w", err)
	}
	if header.ExchangeID, err = cursor.uint16(); err != nil {
		return ProtocolHeader{}, nil, fmt.Errorf("exchange ID: %w", err)
	}
	if flags&exchangeVendor != 0 {
		value, err := cursor.uint16()
		if err != nil {
			return ProtocolHeader{}, nil, fmt.Errorf("protocol vendor ID: %w", err)
		}
		header.VendorID = &value
	}
	if header.ProtocolID, err = cursor.uint16(); err != nil {
		return ProtocolHeader{}, nil, fmt.Errorf("protocol ID: %w", err)
	}
	if flags&exchangeAck != 0 {
		value, err := cursor.uint32()
		if err != nil {
			return ProtocolHeader{}, nil, fmt.Errorf("acknowledged message counter: %w", err)
		}
		header.AckCounter = &value
	}
	if flags&exchangeExtensions != 0 {
		if header.Extensions, err = cursor.lengthPrefixed(); err != nil {
			return ProtocolHeader{}, nil, fmt.Errorf("secured extensions: %w", err)
		}
	}
	return header, cursor.rest(), nil
}

// reader is a bounds-checked little-endian cursor. Matter frames arrive from
// the network, so every read is length-checked before it is performed.
type reader struct {
	data []byte
	pos  int
}

func (r *reader) take(count int) ([]byte, error) {
	if r.pos+count > len(r.data) {
		return nil, fmt.Errorf("need %d more bytes, %d available", count, len(r.data)-r.pos)
	}
	value := r.data[r.pos : r.pos+count]
	r.pos += count
	return value, nil
}

func (r *reader) byteValue() (byte, error) {
	value, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (r *reader) uint16() (uint16, error) {
	value, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(value), nil
}

func (r *reader) uint32() (uint32, error) {
	value, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(value), nil
}

func (r *reader) uint64() (uint64, error) {
	value, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(value), nil
}

func (r *reader) lengthPrefixed() ([]byte, error) {
	length, err := r.uint16()
	if err != nil {
		return nil, err
	}
	return r.take(int(length))
}

func (r *reader) rest() []byte { return r.data[r.pos:] }

// Ptr addresses a value for the optional header fields, so a literal can set
// them inline: Header{SourceNodeID: message.Ptr(uint64(1))}.
func Ptr[T any](value T) *T { return &value }
