package pase

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func repeat(value byte, count int) []byte { return bytes.Repeat([]byte{value}, count) }

// A hand-computed body, so a change in tag numbers or TLV framing cannot slip
// through unnoticed.
func TestPake3WireLayout(t *testing.T) {
	encoded, err := Pake3{CA: repeat(0xAB, 32)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// 15          anonymous structure
	// 30 01 20    context tag 1, octet string, one-byte length 0x20
	// AB*32       the confirmation
	// 18          end of container
	want := "15" + "300120" + strings.Repeat("ab", 32) + "18"
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("encoded %s\n    want %s", got, want)
	}
}

// This is the shape emitted by connectedhomeip's SendPBKDFParamRequest:
// fixed-width uint16 fields plus the default MRP session parameters in tag 5.
// A loopback peer built from this package would accept a mutually wrong shape,
// so keep an independent byte vector for real-device interoperability.
func TestPBKDFParamRequestWireLayout(t *testing.T) {
	encoded, err := PBKDFParamRequest{
		InitiatorRandom: repeat(0x11, RandomSize), InitiatorSessionID: 0x1234,
		PasscodeID: 0, HasPBKDFParameters: false,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := "15" + "300120" + strings.Repeat("11", RandomSize) +
		"25023412" + "25030000" + "2804" +
		"3505" + "2601f4010000" + "26022c010000" + "2503a00f" +
		"25041500" + "25050c00" + "260600010601" + "25070100" + "18" + "18"
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("encoded %s\n    want %s", got, want)
	}
}

func TestRoundTrips(t *testing.T) {
	t.Run("PBKDFParamRequest", func(t *testing.T) {
		original := PBKDFParamRequest{
			InitiatorRandom:    repeat(0x11, RandomSize),
			InitiatorSessionID: 0x1234,
			PasscodeID:         0,
			HasPBKDFParameters: true,
		}
		encoded, err := original.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePBKDFParamRequest(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.InitiatorRandom, original.InitiatorRandom) ||
			decoded.InitiatorSessionID != original.InitiatorSessionID ||
			decoded.PasscodeID != original.PasscodeID ||
			decoded.HasPBKDFParameters != original.HasPBKDFParameters {
			t.Fatalf("round trip gave %+v", decoded)
		}
	})

	t.Run("PBKDFParamResponse with parameters", func(t *testing.T) {
		original := PBKDFParamResponse{
			InitiatorRandom:    repeat(0x22, RandomSize),
			ResponderRandom:    repeat(0x33, RandomSize),
			ResponderSessionID: 0x4321,
			Parameters:         &PBKDFParameters{Iterations: 12000, Salt: repeat(0x44, 20)},
		}
		encoded, err := original.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePBKDFParamResponse(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Parameters == nil {
			t.Fatal("the nested parameter set was lost")
		}
		if decoded.Parameters.Iterations != 12000 || !bytes.Equal(decoded.Parameters.Salt, original.Parameters.Salt) {
			t.Fatalf("parameters round-tripped as %+v", decoded.Parameters)
		}
		if decoded.ResponderSessionID != original.ResponderSessionID ||
			!bytes.Equal(decoded.ResponderRandom, original.ResponderRandom) {
			t.Fatalf("round trip gave %+v", decoded)
		}
	})

	t.Run("PBKDFParamResponse without parameters", func(t *testing.T) {
		// A device may omit them when the commissioner said it has them.
		original := PBKDFParamResponse{
			InitiatorRandom:    repeat(0x22, RandomSize),
			ResponderRandom:    repeat(0x33, RandomSize),
			ResponderSessionID: 7,
		}
		encoded, err := original.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePBKDFParamResponse(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Parameters != nil {
			t.Fatal("parameters appeared out of nowhere")
		}
	})

	t.Run("Pake1", func(t *testing.T) {
		encoded, err := Pake1{PA: repeat(0x04, 65)}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePake1(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.PA, repeat(0x04, 65)) {
			t.Fatal("pA did not round-trip")
		}
	})

	t.Run("Pake2", func(t *testing.T) {
		encoded, err := Pake2{PB: repeat(0x04, 65), CB: repeat(0x55, 32)}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePake2(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.PB, repeat(0x04, 65)) || !bytes.Equal(decoded.CB, repeat(0x55, 32)) {
			t.Fatal("Pake2 did not round-trip")
		}
	})

	t.Run("Pake3", func(t *testing.T) {
		encoded, err := Pake3{CA: repeat(0x66, 32)}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePake3(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.CA, repeat(0x66, 32)) {
			t.Fatal("cA did not round-trip")
		}
	})
}

func TestEncodeRejectsWrongLengths(t *testing.T) {
	if _, err := (PBKDFParamRequest{InitiatorRandom: repeat(1, 31)}).Encode(); err == nil {
		t.Fatal("a 31-byte initiator random was encoded")
	}
	if _, err := (PBKDFParamResponse{
		InitiatorRandom: repeat(1, RandomSize), ResponderRandom: repeat(2, 31),
	}).Encode(); err == nil {
		t.Fatal("a 31-byte responder random was encoded")
	}
	if _, err := (PBKDFParamResponse{
		InitiatorRandom: repeat(1, RandomSize), ResponderRandom: repeat(2, RandomSize),
		Parameters: &PBKDFParameters{Iterations: 1000, Salt: repeat(3, 15)},
	}).Encode(); err == nil {
		t.Fatal("a 15-byte salt was encoded")
	}
	if _, err := (Pake1{PA: repeat(4, 64)}).Encode(); err == nil {
		t.Fatal("a 64-byte share was encoded")
	}
	if _, err := (Pake2{PB: repeat(4, 65), CB: repeat(5, 31)}).Encode(); err == nil {
		t.Fatal("a 31-byte confirmation was encoded")
	}
	if _, err := (Pake3{CA: repeat(6, 33)}).Encode(); err == nil {
		t.Fatal("a 33-byte confirmation was encoded")
	}
}

// Every decoder must insist on its mandatory fields rather than returning a
// half-filled struct.
func TestDecodeRejectsMissingMandatoryFields(t *testing.T) {
	partialRequest := func() []byte {
		var writer tlv.Writer
		writer.StartStructure(tlv.Anonymous())
		writer.PutBytes(tlv.Context(1), repeat(1, RandomSize))
		writer.PutUint(tlv.Context(2), 5)
		writer.EndContainer() // no passcodeId, no hasPBKDFParameters
		encoded, err := writer.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if _, err := DecodePBKDFParamRequest(partialRequest()); err == nil {
		t.Fatal("a request without passcodeId was accepted")
	}

	empty := func() []byte {
		var writer tlv.Writer
		writer.StartStructure(tlv.Anonymous())
		writer.EndContainer()
		encoded, _ := writer.Bytes()
		return encoded
	}()
	for name, decode := range map[string]func([]byte) error{
		"PBKDFParamRequest":  func(b []byte) error { _, err := DecodePBKDFParamRequest(b); return err },
		"PBKDFParamResponse": func(b []byte) error { _, err := DecodePBKDFParamResponse(b); return err },
		"Pake1":              func(b []byte) error { _, err := DecodePake1(b); return err },
		"Pake2":              func(b []byte) error { _, err := DecodePake2(b); return err },
		"Pake3":              func(b []byte) error { _, err := DecodePake3(b); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := decode(empty); err == nil {
				t.Fatal("an empty structure was accepted")
			}
		})
	}
}

func TestDecodeRejectsWrongTypesAndLengths(t *testing.T) {
	cases := map[string]func() []byte{
		"initiator random is an integer": func() []byte {
			var writer tlv.Writer
			writer.StartStructure(tlv.Anonymous())
			writer.PutUint(tlv.Context(1), 42)
			writer.PutUint(tlv.Context(2), 1)
			writer.PutUint(tlv.Context(3), 0)
			writer.PutBool(tlv.Context(4), false)
			writer.EndContainer()
			encoded, _ := writer.Bytes()
			return encoded
		},
		"initiator random is too short": func() []byte {
			var writer tlv.Writer
			writer.StartStructure(tlv.Anonymous())
			writer.PutBytes(tlv.Context(1), repeat(1, 16))
			writer.PutUint(tlv.Context(2), 1)
			writer.PutUint(tlv.Context(3), 0)
			writer.PutBool(tlv.Context(4), false)
			writer.EndContainer()
			encoded, _ := writer.Bytes()
			return encoded
		},
		"session ID exceeds 16 bits": func() []byte {
			var writer tlv.Writer
			writer.StartStructure(tlv.Anonymous())
			writer.PutBytes(tlv.Context(1), repeat(1, RandomSize))
			writer.PutUint(tlv.Context(2), 0x10000)
			writer.PutUint(tlv.Context(3), 0)
			writer.PutBool(tlv.Context(4), false)
			writer.EndContainer()
			encoded, _ := writer.Bytes()
			return encoded
		},
		"hasPBKDFParameters is an integer": func() []byte {
			var writer tlv.Writer
			writer.StartStructure(tlv.Anonymous())
			writer.PutBytes(tlv.Context(1), repeat(1, RandomSize))
			writer.PutUint(tlv.Context(2), 1)
			writer.PutUint(tlv.Context(3), 0)
			writer.PutUint(tlv.Context(4), 1)
			writer.EndContainer()
			encoded, _ := writer.Bytes()
			return encoded
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if decoded, err := DecodePBKDFParamRequest(build()); err == nil {
				t.Fatalf("accepted as %+v", decoded)
			}
		})
	}

	t.Run("pbkdf_parameters is not a structure", func(t *testing.T) {
		var writer tlv.Writer
		writer.StartStructure(tlv.Anonymous())
		writer.PutBytes(tlv.Context(1), repeat(1, RandomSize))
		writer.PutBytes(tlv.Context(2), repeat(2, RandomSize))
		writer.PutUint(tlv.Context(3), 1)
		writer.PutUint(tlv.Context(4), 99)
		writer.EndContainer()
		encoded, _ := writer.Bytes()
		if _, err := DecodePBKDFParamResponse(encoded); err == nil {
			t.Fatal("a scalar pbkdf_parameters was accepted")
		}
	})

	t.Run("pbkdf_parameters missing its salt", func(t *testing.T) {
		var writer tlv.Writer
		writer.StartStructure(tlv.Anonymous())
		writer.PutBytes(tlv.Context(1), repeat(1, RandomSize))
		writer.PutBytes(tlv.Context(2), repeat(2, RandomSize))
		writer.PutUint(tlv.Context(3), 1)
		writer.StartStructure(tlv.Context(4))
		writer.PutUint(tlv.Context(1), 1000)
		writer.EndContainer()
		writer.EndContainer()
		encoded, _ := writer.Bytes()
		if _, err := DecodePBKDFParamResponse(encoded); err == nil {
			t.Fatal("parameters without a salt were accepted")
		}
	})
}

// Newer peers may add fields we do not know. Skipping them must not
// desynchronize the reader, including when the unknown field is a container.
func TestUnknownFieldsAreTolerated(t *testing.T) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBytes(tlv.Context(1), repeat(0x11, RandomSize))
	writer.PutBytes(tlv.Context(2), repeat(0x22, RandomSize))
	writer.PutUint(tlv.Context(3), 0x0501)
	writer.StartStructure(tlv.Context(4))
	writer.PutUint(tlv.Context(1), 2000)
	writer.PutBytes(tlv.Context(2), repeat(0x33, 16))
	writer.EndContainer()
	// An unknown nested container, of the shape a future SED parameter set
	// would take.
	writer.StartStructure(tlv.Context(5))
	writer.PutUint(tlv.Context(1), 300)
	writer.StartArray(tlv.Context(2))
	writer.PutUint(tlv.Anonymous(), 1)
	writer.EndContainer()
	writer.EndContainer()
	writer.PutUint(tlv.Context(9), 12345) // an unknown scalar
	writer.EndContainer()
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodePBKDFParamResponse(encoded)
	if err != nil {
		t.Fatalf("unknown fields broke decoding: %v", err)
	}
	if decoded.ResponderSessionID != 0x0501 {
		t.Fatalf("session ID decoded as %d", decoded.ResponderSessionID)
	}
	if decoded.Parameters == nil || decoded.Parameters.Iterations != 2000 {
		t.Fatalf("known fields were lost: %+v", decoded.Parameters)
	}
}

func TestDecodeRejectsNonStructureBodies(t *testing.T) {
	var writer tlv.Writer
	writer.PutUint(tlv.Anonymous(), 5)
	scalar, _ := writer.Bytes()
	for name, decode := range map[string]func([]byte) error{
		"scalar body": func(b []byte) error { _, err := DecodePake1(b); return err },
		"empty body":  func(b []byte) error { _, err := DecodePake1(nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := decode(scalar); err == nil {
				t.Fatal("a non-structure body was accepted")
			}
		})
	}
}

func TestStatusReportLayout(t *testing.T) {
	report := StatusReport{GeneralCode: 1, ProtocolID: 0x00000000, ProtocolCode: 0x0002}
	// general code LE | protocol ID LE | protocol code LE
	want := "0100" + "00000000" + "0200"
	if got := hex.EncodeToString(report.Encode()); got != want {
		t.Fatalf("encoded %s, want %s", got, want)
	}
	decoded, err := DecodeStatusReport(report.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GeneralCode != 1 || decoded.ProtocolCode != 0x0002 {
		t.Fatalf("decoded %+v", decoded)
	}
	if decoded.OK() {
		t.Fatal("a failure report reported OK")
	}
	if !SessionEstablished().OK() {
		t.Fatal("the success report did not report OK")
	}
	if Failure(StatusInvalidParameter).OK() {
		t.Fatal("a failure report reported OK")
	}
	if _, err := DecodeStatusReport(make([]byte, 7)); err == nil {
		t.Fatal("a 7-byte status report was accepted")
	}
	// Trailing protocol data is preserved.
	withData := StatusReport{GeneralCode: 1, ProtocolCode: 4, Data: []byte{0xDE, 0xAD}}
	roundTripped, err := DecodeStatusReport(withData.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTripped.Data, []byte{0xDE, 0xAD}) {
		t.Fatalf("protocol data came back as %X", roundTripped.Data)
	}
}

func TestDefaultParametersAreWithinSpecification(t *testing.T) {
	first, err := DefaultParameters()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Salt) < 16 || len(first.Salt) > 32 {
		t.Fatalf("salt is %d bytes, outside 16..32", len(first.Salt))
	}
	if first.Iterations < 1000 || first.Iterations > 100000 {
		t.Fatalf("iteration count %d is outside 1000..100000", first.Iterations)
	}
	second, err := DefaultParameters()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Salt, second.Salt) {
		t.Fatal("the salt must be fresh for every device")
	}
}
