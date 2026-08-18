package message

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/crypto"
)

var sessionKey = bytes.Repeat([]byte{0x5A}, 16)

// A hand-computed frame, byte for byte, so a silent field-order or
// endianness change cannot pass unnoticed.
func TestUnsecuredWireLayout(t *testing.T) {
	msg := Message{
		Header: Header{Counter: 1},
		Protocol: ProtocolHeader{
			Initiator: true, Reliable: true,
			Opcode: OpcodePBKDFParamRequest, ExchangeID: 0x1234,
			ProtocolID: ProtocolSecureChannel,
		},
	}
	encoded, err := msg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// flags 00 | session 0000 | security 00 | counter 01000000
	// exchange flags 05 (initiator+reliable) | opcode 20 | exchange 3412 | protocol 0000
	want := "00" + "0000" + "00" + "01000000" + "05" + "20" + "3412" + "0000"
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("encoded %s\n    want %s", got, want)
	}

	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Protocol.Initiator || !parsed.Protocol.Reliable {
		t.Fatalf("exchange flags lost: %+v", parsed.Protocol)
	}
	if parsed.Protocol.Opcode != OpcodePBKDFParamRequest || parsed.Protocol.ExchangeID != 0x1234 {
		t.Fatalf("protocol header lost: %+v", parsed.Protocol)
	}
}

func TestSourceNodeIDIsLittleEndian(t *testing.T) {
	msg := Message{
		Header:   Header{Counter: 1, SourceNodeID: Ptr(uint64(0x0102030405060708))},
		Protocol: ProtocolHeader{ProtocolID: ProtocolSecureChannel},
	}
	encoded, err := msg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := "04" + "0000" + "00" + "01000000" + "0807060504030201"
	if got := hex.EncodeToString(encoded[:16]); got != want {
		t.Fatalf("header %s\n   want %s", got, want)
	}
}

func TestRoundTripFieldCombinations(t *testing.T) {
	payload := []byte{0x15, 0x18} // an empty TLV structure
	cases := []struct {
		name string
		msg  Message
	}{
		{"minimal", Message{Protocol: ProtocolHeader{ProtocolID: ProtocolSecureChannel}}},
		{"source and destination node", Message{
			Header: Header{
				Counter:           7,
				SourceNodeID:      Ptr(uint64(0xFFEEDDCCBBAA9988)),
				DestinationNodeID: Ptr(uint64(0x1122334455667788)),
			},
			Protocol: ProtocolHeader{Opcode: OpcodePASEPake1, ProtocolID: ProtocolSecureChannel},
			Payload:  payload,
		}},
		{"group session", Message{
			Header: Header{
				SessionType:        SessionGroup,
				Counter:            9,
				SourceNodeID:       Ptr(uint64(2)),
				DestinationGroupID: Ptr(uint16(0xBEEF)),
			},
			Protocol: ProtocolHeader{ProtocolID: ProtocolInteractionModel},
		}},
		{"control message", Message{
			Header:   Header{Control: true, Counter: 3, SourceNodeID: Ptr(uint64(4))},
			Protocol: ProtocolHeader{ProtocolID: ProtocolSecureChannel},
		}},
		{"acknowledgement", Message{
			Header: Header{Counter: 11},
			Protocol: ProtocolHeader{
				Initiator: true, Reliable: true, AckCounter: Ptr(uint32(10)),
				Opcode: OpcodeStandaloneAck, ProtocolID: ProtocolSecureChannel,
			},
		}},
		{"vendor protocol", Message{
			Header: Header{Counter: 12},
			Protocol: ProtocolHeader{
				VendorID: Ptr(uint16(0xFFF1)), ProtocolID: 0x0042, Opcode: 0x99,
			},
			Payload: payload,
		}},
		{"both extension blobs", Message{
			Header:   Header{Counter: 13, Extensions: []byte{0xAA, 0xBB}},
			Protocol: ProtocolHeader{ProtocolID: ProtocolBDX, Extensions: []byte{0xCC}},
			Payload:  payload,
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Group sessions are never unsecured, so only Seal applies there.
			if testCase.msg.Header.SessionType == SessionUnicast {
				encoded, err := testCase.msg.Encode()
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := Parse(encoded)
				if err != nil {
					t.Fatal(err)
				}
				assertSameMessage(t, parsed, testCase.msg)
			}

			sealed := testCase.msg
			sealed.Header.SessionID = 0x0501
			frame, err := sealed.Seal(sessionKey)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(frame, sessionKey)
			if err != nil {
				t.Fatal(err)
			}
			assertSameMessage(t, opened, sealed)
		})
	}
}

func assertSameMessage(t *testing.T, got, want Message) {
	t.Helper()
	if !reflect.DeepEqual(got.Header, want.Header) {
		t.Fatalf("header\n got %+v\nwant %+v", got.Header, want.Header)
	}
	if !reflect.DeepEqual(got.Protocol, want.Protocol) {
		t.Fatalf("protocol\n got %+v\nwant %+v", got.Protocol, want.Protocol)
	}
	if len(got.Payload) != len(want.Payload) || (len(want.Payload) > 0 && !bytes.Equal(got.Payload, want.Payload)) {
		t.Fatalf("payload got %X, want %X", got.Payload, want.Payload)
	}
}

// The protocol header must be inside the ciphertext: an observer without the
// key must not be able to read the opcode or exchange ID.
func TestProtocolHeaderIsEncrypted(t *testing.T) {
	msg := Message{
		Header:   Header{SessionID: 0x0501, Counter: 1, SourceNodeID: Ptr(uint64(0x1122334455667788))},
		Protocol: ProtocolHeader{Opcode: 0xA7, ExchangeID: 0x3C3C, ProtocolID: ProtocolInteractionModel},
		Payload:  []byte("secret attribute report"),
	}
	frame, err := msg.Seal(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame, []byte("secret attribute report")) {
		t.Fatal("payload appears in the clear")
	}
	if bytes.Contains(frame[8:], []byte{0xA7}) && bytes.Contains(frame[8:], []byte{0x3C, 0x3C}) {
		t.Fatal("protocol header appears to be unencrypted")
	}
	// The header itself does stay readable: that is what routing needs.
	if frame[1] != 0x01 || frame[2] != 0x05 {
		t.Fatalf("session ID is not readable in the clear: % X", frame[:4])
	}
}

// The cleartext header is additional data, so any tampering must be caught.
func TestHeaderIsAuthenticated(t *testing.T) {
	msg := Message{
		Header:   Header{SessionID: 0x0501, Counter: 42, SourceNodeID: Ptr(uint64(9))},
		Protocol: ProtocolHeader{Opcode: OpcodePASEPake2, ProtocolID: ProtocolSecureChannel},
		Payload:  []byte{0x15, 0x18},
	}
	frame, err := msg.Seal(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(frame, sessionKey); err != nil {
		t.Fatal(err)
	}
	for index := range frame {
		tampered := bytes.Clone(frame)
		tampered[index] ^= 0x01
		if _, err := Open(tampered, sessionKey); err == nil {
			t.Fatalf("flipping a bit at offset %d went undetected", index)
		}
	}
	if _, err := Open(frame, bytes.Repeat([]byte{0x5B}, 16)); err == nil {
		t.Fatal("the wrong key opened the message")
	}
}

// The nonce is security flags, message counter and source node ID. Rebuild
// it by hand and decrypt with the raw AEAD to prove the binding.
func TestNonceConstruction(t *testing.T) {
	source := uint64(0x0102030405060708)
	msg := Message{
		Header: Header{
			SessionID: 0x1234, Control: true, Counter: 0xAABBCCDD, SourceNodeID: &source,
		},
		Protocol: ProtocolHeader{Opcode: OpcodePASEPake3, ProtocolID: ProtocolSecureChannel},
		Payload:  []byte("payload"),
	}
	frame, err := msg.Seal(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	headerLength := 1 + 2 + 1 + 4 + 8
	header, ciphertext := frame[:headerLength], frame[headerLength:]

	var expected [crypto.NonceSize]byte
	expected[0] = securityControl // control message, unicast session
	binary.LittleEndian.PutUint32(expected[1:5], 0xAABBCCDD)
	binary.LittleEndian.PutUint64(expected[5:13], source)

	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := crypto.NewCCM(block, crypto.TagSize, crypto.NonceSize)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := aead.Open(nil, expected[:], ciphertext, header)
	if err != nil {
		t.Fatalf("the hand-built nonce did not decrypt the frame: %v", err)
	}
	if !bytes.HasSuffix(plaintext, []byte("payload")) {
		t.Fatalf("decrypted %X", plaintext)
	}
}

func TestRejectsMalformedFrames(t *testing.T) {
	cases := map[string]string{
		"empty":                       "",
		"truncated header":            "000000",
		"missing source node ID":      "04" + "0000" + "00" + "01000000" + "0807",
		"reserved destination size":   "03" + "0000" + "00" + "01000000",
		"unsupported format version":  "10" + "0000" + "00" + "01000000",
		"reserved message flag bit":   "08" + "0000" + "00" + "01000000",
		"reserved security flag bit":  "00" + "0000" + "04" + "01000000",
		"privacy enhanced":            "00" + "0000" + "80" + "01000000",
		"reserved session type":       "00" + "0000" + "02" + "01000000" + "0000" + "0000" + "0000",
		"truncated protocol header":   "00" + "0000" + "00" + "01000000" + "0520",
		"extension longer than frame": "00" + "0000" + "20" + "01000000" + "FF00",
		"reserved exchange flag bit":  "00" + "0000" + "00" + "01000000" + "20" + "20" + "3412" + "0000",
		"secured session without MIC": "00" + "0100" + "00" + "01000000" + "0520" + "34120000",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			wire, err := hex.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if msg, err := Parse(wire); err == nil {
				t.Fatalf("accepted malformed frame as %+v", msg)
			}
		})
	}
}

func TestEncodeRejectsInvalidCombinations(t *testing.T) {
	cases := map[string]Message{
		"two destinations": {Header: Header{
			DestinationNodeID:  Ptr(uint64(1)),
			DestinationGroupID: Ptr(uint16(2)),
		}},
		"reserved session type":     {Header: Header{SessionType: 3}},
		"group without a group ID":  {Header: Header{SessionType: SessionGroup}},
		"unsecured with session ID": {Header: Header{SessionID: 5}},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := msg.Encode(); err == nil {
				t.Fatal("invalid message was encoded")
			}
		})
	}
}

func TestRejectsWrongKeySize(t *testing.T) {
	msg := Message{Header: Header{SessionID: 1, Counter: 1}}
	if _, err := msg.Seal(make([]byte, 32)); err == nil {
		t.Fatal("a 32-byte key must be rejected: Matter session keys are 128-bit")
	}
	if _, err := Open(bytes.Repeat([]byte{0}, 40), make([]byte, 15)); err == nil {
		t.Fatal("a 15-byte key must be rejected")
	}
}
