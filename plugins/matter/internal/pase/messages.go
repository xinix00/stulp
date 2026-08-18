// Package pase implements Matter's Passcode Authenticated Session
// Establishment: the exchange that turns a pairing-code passcode into an
// encrypted session. It carries the TLV bodies of the five PASE messages and
// drives both roles of the handshake.
package pase

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// RandomSize is the length of the per-role random each side contributes.
const RandomSize = 32

const (
	defaultIdleRetransTimeout   uint32 = 500
	defaultActiveRetransTimeout uint32 = 300
	defaultActiveThreshold      uint16 = 4000
	dataModelRevision           uint16 = 21
	interactionModelRevision    uint16 = 12
	specificationVersion        uint32 = 0x01060100
	maxPathsPerInvoke           uint16 = 1
)

// PBKDFParamRequest opens commissioning: the commissioner announces the
// session ID it wants to use and asks for the device's PBKDF parameters.
type PBKDFParamRequest struct {
	InitiatorRandom    []byte
	InitiatorSessionID uint16
	// PasscodeID selects which passcode to use; 0 is the commissioning one.
	PasscodeID uint16
	// HasPBKDFParameters tells the device the commissioner already knows the
	// salt and iteration count, so it may leave them out of its response.
	HasPBKDFParameters bool
}

// PBKDFParameters are the inputs that expand a passcode into SPAKE2+
// scalars.
type PBKDFParameters struct {
	Iterations uint32
	Salt       []byte
}

// PBKDFParamResponse answers with the device's session ID and, unless the
// commissioner said it already had them, the PBKDF parameters.
type PBKDFParamResponse struct {
	InitiatorRandom    []byte
	ResponderRandom    []byte
	ResponderSessionID uint16
	Parameters         *PBKDFParameters
}

// Pake1 carries the commissioner's SPAKE2+ share.
type Pake1 struct{ PA []byte }

// Pake2 carries the device's share and its confirmation.
type Pake2 struct{ PB, CB []byte }

// Pake3 carries the commissioner's confirmation.
type Pake3 struct{ CA []byte }

// Encode serializes the request.
func (m PBKDFParamRequest) Encode() ([]byte, error) {
	if len(m.InitiatorRandom) != RandomSize {
		return nil, fmt.Errorf("initiator random must be %d bytes, got %d", RandomSize, len(m.InitiatorRandom))
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), m.InitiatorRandom)
	writer.PutUintWidth(tlv.Context(2), uint64(m.InitiatorSessionID), 2)
	writer.PutUintWidth(tlv.Context(3), uint64(m.PasscodeID), 2)
	writer.PutBool(tlv.Context(4), m.HasPBKDFParameters)
	putDefaultSessionParameters(&writer, tlv.Context(5))
	writer.EndContainer()
	return writer.Bytes()
}

// DecodePBKDFParamRequest parses the request.
func DecodePBKDFParamRequest(data []byte) (PBKDFParamRequest, error) {
	var result PBKDFParamRequest
	var seen fieldSet
	reader, err := openStructure(data)
	if err != nil {
		return result, err
	}
	for {
		tag, element, ok, err := nextField(reader)
		if err != nil {
			return result, err
		}
		if !ok {
			break
		}
		seen.mark(tag)
		switch tag {
		case 1:
			result.InitiatorRandom, err = octets(element, RandomSize, RandomSize)
		case 2:
			result.InitiatorSessionID, err = uint16Field(element)
		case 3:
			result.PasscodeID, err = uint16Field(element)
		case 4:
			if element.Type != tlv.TypeBool {
				err = fmt.Errorf("hasPBKDFParameters must be a boolean")
			}
			result.HasPBKDFParameters = element.Bool
		default:
			// Session parameters (tag 5) and future fields do not affect the
			// PASE key schedule. Consume nested values so ignoring them cannot
			// desynchronize the enclosing request parser.
			if err = skipContainer(reader, element); err != nil {
				return PBKDFParamRequest{}, err
			}
		}
		if err != nil {
			return PBKDFParamRequest{}, fmt.Errorf("PBKDFParamRequest field %d: %w", tag, err)
		}
	}
	if err := seen.require("PBKDFParamRequest", 1, 2, 3, 4); err != nil {
		return PBKDFParamRequest{}, err
	}
	return result, nil
}

// Encode serializes the response.
func (m PBKDFParamResponse) Encode() ([]byte, error) {
	if len(m.InitiatorRandom) != RandomSize {
		return nil, fmt.Errorf("initiator random must be %d bytes, got %d", RandomSize, len(m.InitiatorRandom))
	}
	if len(m.ResponderRandom) != RandomSize {
		return nil, fmt.Errorf("responder random must be %d bytes, got %d", RandomSize, len(m.ResponderRandom))
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), m.InitiatorRandom)
	writer.PutBytes(tlv.Context(2), m.ResponderRandom)
	writer.PutUintWidth(tlv.Context(3), uint64(m.ResponderSessionID), 2)
	if m.Parameters != nil {
		if len(m.Parameters.Salt) < 16 || len(m.Parameters.Salt) > 32 {
			return nil, fmt.Errorf("PBKDF salt must be 16..32 bytes, got %d", len(m.Parameters.Salt))
		}
		writer.StartStructure(tlv.Context(4))
		writer.PutUintWidth(tlv.Context(1), uint64(m.Parameters.Iterations), 4)
		writer.PutBytes(tlv.Context(2), m.Parameters.Salt)
		writer.EndContainer()
	}
	putDefaultSessionParameters(&writer, tlv.Context(5))
	writer.EndContainer()
	return writer.Bytes()
}

// putDefaultSessionParameters mirrors connectedhomeip's default
// ReliableMessageProtocolConfig. Matter session establishment exchanges carry
// these values even when the defaults are used, so peers know how quickly this
// controller can acknowledge traffic.
func putDefaultSessionParameters(writer *tlv.Writer, tag tlv.Tag) {
	writer.StartStructure(tag)
	writer.PutUintWidth(tlv.Context(1), uint64(defaultIdleRetransTimeout), 4)
	writer.PutUintWidth(tlv.Context(2), uint64(defaultActiveRetransTimeout), 4)
	writer.PutUintWidth(tlv.Context(3), uint64(defaultActiveThreshold), 2)
	writer.PutUintWidth(tlv.Context(4), uint64(dataModelRevision), 2)
	writer.PutUintWidth(tlv.Context(5), uint64(interactionModelRevision), 2)
	writer.PutUintWidth(tlv.Context(6), uint64(specificationVersion), 4)
	writer.PutUintWidth(tlv.Context(7), uint64(maxPathsPerInvoke), 2)
	writer.EndContainer()
}

// DecodePBKDFParamResponse parses the response, including the nested
// parameter set when the device sent one.
func DecodePBKDFParamResponse(data []byte) (PBKDFParamResponse, error) {
	var result PBKDFParamResponse
	var seen fieldSet
	reader, err := openStructure(data)
	if err != nil {
		return result, err
	}
	for {
		tag, element, ok, err := nextField(reader)
		if err != nil {
			return result, err
		}
		if !ok {
			break
		}
		seen.mark(tag)
		switch tag {
		case 1:
			result.InitiatorRandom, err = octets(element, RandomSize, RandomSize)
		case 2:
			result.ResponderRandom, err = octets(element, RandomSize, RandomSize)
		case 3:
			result.ResponderSessionID, err = uint16Field(element)
		case 4:
			if element.Type != tlv.TypeStructure {
				err = errors.New("pbkdf_parameters must be a structure")
				break
			}
			var parameters PBKDFParameters
			parameters, err = readParameters(reader)
			result.Parameters = &parameters
		default:
			if err = skipContainer(reader, element); err != nil {
				return PBKDFParamResponse{}, err
			}
		}
		if err != nil {
			return PBKDFParamResponse{}, fmt.Errorf("PBKDFParamResponse field %d: %w", tag, err)
		}
	}
	if err := seen.require("PBKDFParamResponse", 1, 2, 3); err != nil {
		return PBKDFParamResponse{}, err
	}
	return result, nil
}

func readParameters(reader *tlv.Reader) (PBKDFParameters, error) {
	var result PBKDFParameters
	var seen fieldSet
	for {
		tag, element, ok, err := nextField(reader)
		if err != nil {
			return result, err
		}
		if !ok {
			break
		}
		seen.mark(tag)
		switch tag {
		case 1:
			if element.Type != tlv.TypeUint {
				err = errors.New("iterations must be an unsigned integer")
				break
			}
			if element.Uint > 0xFFFFFFFF {
				err = fmt.Errorf("iteration count %d exceeds 32 bits", element.Uint)
				break
			}
			result.Iterations = uint32(element.Uint)
		case 2:
			result.Salt, err = octets(element, 16, 32)
		default:
			if err = skipContainer(reader, element); err != nil {
				return result, err
			}
		}
		if err != nil {
			return PBKDFParameters{}, fmt.Errorf("pbkdf_parameters field %d: %w", tag, err)
		}
	}
	return result, seen.require("pbkdf_parameters", 1, 2)
}

// Encode serializes Pake1.
func (m Pake1) Encode() ([]byte, error) { return encodeSingleOctets(1, m.PA, 65, 65) }

// DecodePake1 parses Pake1.
func DecodePake1(data []byte) (Pake1, error) {
	value, err := decodeSingleOctets(data, "Pake1", 1, 65, 65)
	return Pake1{PA: value}, err
}

// Encode serializes Pake2.
func (m Pake2) Encode() ([]byte, error) {
	if len(m.PB) != 65 {
		return nil, fmt.Errorf("pB must be 65 bytes, got %d", len(m.PB))
	}
	if len(m.CB) != 32 {
		return nil, fmt.Errorf("cB must be 32 bytes, got %d", len(m.CB))
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), m.PB)
	writer.PutBytes(tlv.Context(2), m.CB)
	writer.EndContainer()
	return writer.Bytes()
}

// DecodePake2 parses Pake2.
func DecodePake2(data []byte) (Pake2, error) {
	var result Pake2
	var seen fieldSet
	reader, err := openStructure(data)
	if err != nil {
		return result, err
	}
	for {
		tag, element, ok, err := nextField(reader)
		if err != nil {
			return result, err
		}
		if !ok {
			break
		}
		seen.mark(tag)
		switch tag {
		case 1:
			result.PB, err = octets(element, 65, 65)
		case 2:
			result.CB, err = octets(element, 32, 32)
		default:
			if err = skipContainer(reader, element); err != nil {
				return Pake2{}, err
			}
		}
		if err != nil {
			return Pake2{}, fmt.Errorf("Pake2 field %d: %w", tag, err)
		}
	}
	return result, seen.require("Pake2", 1, 2)
}

// Encode serializes Pake3.
func (m Pake3) Encode() ([]byte, error) { return encodeSingleOctets(1, m.CA, 32, 32) }

// DecodePake3 parses Pake3.
func DecodePake3(data []byte) (Pake3, error) {
	value, err := decodeSingleOctets(data, "Pake3", 1, 32, 32)
	return Pake3{CA: value}, err
}

// StatusReport ends a session-establishment exchange. It is deliberately not
// TLV: the specification defines it as a fixed little-endian layout so it can
// be produced even when TLV encoding is what failed.
type StatusReport struct {
	GeneralCode  uint16
	ProtocolID   uint32
	ProtocolCode uint16
	Data         []byte
}

// General codes.
const (
	GeneralSuccess uint16 = 0
	GeneralFailure uint16 = 1
)

// Secure Channel protocol codes.
const (
	StatusSessionEstablishmentSuccess uint16 = 0x0000
	StatusNoSharedTrustRoots          uint16 = 0x0001
	StatusInvalidParameter            uint16 = 0x0002
	StatusCloseSession                uint16 = 0x0003
	StatusBusy                        uint16 = 0x0004
)

// SessionEstablished is the report that ends a successful PASE exchange.
func SessionEstablished() StatusReport {
	return StatusReport{GeneralCode: GeneralSuccess, ProtocolCode: StatusSessionEstablishmentSuccess}
}

// Failure builds a rejection carrying the given Secure Channel code.
func Failure(protocolCode uint16) StatusReport {
	return StatusReport{GeneralCode: GeneralFailure, ProtocolCode: protocolCode}
}

// OK reports whether the peer accepted the session.
func (s StatusReport) OK() bool {
	return s.GeneralCode == GeneralSuccess && s.ProtocolCode == StatusSessionEstablishmentSuccess
}

func (s StatusReport) Error() string {
	if s.ProtocolCode == StatusBusy {
		return fmt.Sprintf("peer is busy (general code %d, protocol code 0x%04x)", s.GeneralCode, s.ProtocolCode)
	}
	return fmt.Sprintf("peer reported general code %d, protocol code 0x%04x", s.GeneralCode, s.ProtocolCode)
}

// Encode serializes the report.
func (s StatusReport) Encode() []byte {
	out := make([]byte, 0, 8+len(s.Data))
	out = binary.LittleEndian.AppendUint16(out, s.GeneralCode)
	out = binary.LittleEndian.AppendUint32(out, s.ProtocolID)
	out = binary.LittleEndian.AppendUint16(out, s.ProtocolCode)
	return append(out, s.Data...)
}

// DecodeStatusReport parses the report.
func DecodeStatusReport(data []byte) (StatusReport, error) {
	if len(data) < 8 {
		return StatusReport{}, fmt.Errorf("status report is %d bytes, want at least 8", len(data))
	}
	return StatusReport{
		GeneralCode:  binary.LittleEndian.Uint16(data[0:2]),
		ProtocolID:   binary.LittleEndian.Uint32(data[2:6]),
		ProtocolCode: binary.LittleEndian.Uint16(data[6:8]),
		Data:         data[8:],
	}, nil
}

// --- TLV helpers -------------------------------------------------------

func encodeSingleOctets(tag uint8, value []byte, min, max int) ([]byte, error) {
	if len(value) < min || len(value) > max {
		return nil, fmt.Errorf("field %d must be %d..%d bytes, got %d", tag, min, max, len(value))
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(tag), value)
	writer.EndContainer()
	return writer.Bytes()
}

func decodeSingleOctets(data []byte, message string, want uint8, min, max int) ([]byte, error) {
	var result []byte
	var seen fieldSet
	reader, err := openStructure(data)
	if err != nil {
		return nil, err
	}
	for {
		tag, element, ok, err := nextField(reader)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		seen.mark(tag)
		if tag == want {
			if result, err = octets(element, min, max); err != nil {
				return nil, fmt.Errorf("%s field %d: %w", message, tag, err)
			}
			continue
		}
		if err := skipContainer(reader, element); err != nil {
			return nil, err
		}
	}
	return result, seen.require(message, want)
}

// openStructure consumes the anonymous structure every PASE body starts with.
func openStructure(data []byte) (*tlv.Reader, error) {
	reader := tlv.NewReader(data)
	element, err := reader.Next()
	if err != nil {
		return nil, fmt.Errorf("read message body: %w", err)
	}
	if element.Type != tlv.TypeStructure {
		return nil, errors.New("message body must be a TLV structure")
	}
	return reader, nil
}

// nextField returns the next context-tagged member, or ok=false at the end of
// the enclosing container.
func nextField(reader *tlv.Reader) (uint8, tlv.Element, bool, error) {
	element, err := reader.Next()
	if errors.Is(err, io.EOF) {
		return 0, tlv.Element{}, false, nil
	}
	if err != nil {
		return 0, tlv.Element{}, false, err
	}
	if element.Type == tlv.TypeEnd {
		return 0, tlv.Element{}, false, nil
	}
	tag, ok := element.Tag.ContextNumber()
	if !ok {
		return 0, tlv.Element{}, false, fmt.Errorf("message member has a %s tag, want a context tag", element.Tag)
	}
	return tag, element, true, nil
}

// skipContainer consumes the body of an unknown container member, so that
// tolerating unknown fields does not desynchronize the reader.
func skipContainer(reader *tlv.Reader, element tlv.Element) error {
	switch element.Type {
	case tlv.TypeStructure, tlv.TypeArray, tlv.TypeList:
	default:
		return nil
	}
	depth := 1
	for depth > 0 {
		next, err := reader.Next()
		if err != nil {
			return fmt.Errorf("skip unknown container: %w", err)
		}
		switch next.Type {
		case tlv.TypeStructure, tlv.TypeArray, tlv.TypeList:
			depth++
		case tlv.TypeEnd:
			depth--
		}
	}
	return nil
}

func octets(element tlv.Element, min, max int) ([]byte, error) {
	if element.Type != tlv.TypeBytes {
		return nil, errors.New("field must be an octet string")
	}
	if len(element.Data) < min || len(element.Data) > max {
		return nil, fmt.Errorf("field is %d bytes, want %d..%d", len(element.Data), min, max)
	}
	return element.Data, nil
}

func uint16Field(element tlv.Element) (uint16, error) {
	if element.Type != tlv.TypeUint {
		return 0, errors.New("field must be an unsigned integer")
	}
	if element.Uint > 0xFFFF {
		return 0, fmt.Errorf("value %d exceeds 16 bits", element.Uint)
	}
	return uint16(element.Uint), nil
}

// fieldSet tracks which context tags a message carried, so decoders can
// insist on the mandatory ones.
type fieldSet uint64

func (f *fieldSet) mark(tag uint8) {
	if tag < 64 {
		*f |= 1 << tag
	}
}

func (f fieldSet) require(message string, tags ...uint8) error {
	for _, tag := range tags {
		if tag >= 64 || f&(1<<tag) == 0 {
			return fmt.Errorf("%s is missing mandatory field %d", message, tag)
		}
	}
	return nil
}
