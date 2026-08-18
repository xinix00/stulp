// Package onboarding parses and generates Matter onboarding payloads: the
// "MT:" QR code and the 11/21-digit manual pairing code. These carry the
// passcode and discriminator a controller needs to commission a device — the
// exact code Apple Home shows when sharing an already-commissioned device
// with another ecosystem.
package onboarding

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// Payload is the decoded content of an onboarding code.
type Payload struct {
	Version   uint8  `json:"version"`
	VendorID  uint16 `json:"vendorId"`
	ProductID uint16 `json:"productId"`
	// CustomFlow: 0 standard, 1 user intent, 2 custom commissioning flow.
	CustomFlow uint8 `json:"customFlow"`
	// Discovery is the discovery-capabilities bitmask (bit 0 SoftAP,
	// bit 1 BLE, bit 2 on-network). Only present in QR payloads.
	Discovery uint8 `json:"discoveryCapabilities"`
	// Discriminator is the full 12-bit discriminator. Manual pairing codes
	// only carry the 4 most significant bits; then ShortDiscriminator is
	// true and the low 8 bits are zero.
	Discriminator      uint16 `json:"discriminator"`
	ShortDiscriminator bool   `json:"shortDiscriminatorOnly,omitempty"`
	Passcode           uint32 `json:"passcode"`
	// Extensions is the decoded optional TLV section of a QR payload
	// (serial number, vendor-specific data), keyed by TLV tag. Display
	// only; extensionBytes carries the raw TLV for faithful re-encoding.
	Extensions     any `json:"extensions,omitempty"`
	extensionBytes []byte
}

const qrPrefix = "MT:"

// Parse decodes either payload form: "MT:..." QR content or a manual pairing
// code (separators allowed).
func Parse(code string) (Payload, error) {
	trimmed := strings.TrimSpace(code)
	if strings.HasPrefix(strings.ToUpper(trimmed), qrPrefix) {
		return ParseQR(trimmed)
	}
	return ParseManualCode(trimmed)
}

var invalidPasscodes = map[uint32]bool{
	0: true, 11111111: true, 22222222: true, 33333333: true, 44444444: true,
	55555555: true, 66666666: true, 77777777: true, 88888888: true,
	99999999: true, 12345678: true, 87654321: true,
}

func validatePasscode(passcode uint32) error {
	if passcode >= 1<<27 {
		return fmt.Errorf("passcode %d does not fit 27 bits", passcode)
	}
	if invalidPasscodes[passcode] {
		return fmt.Errorf("passcode %08d is forbidden by the Matter specification", passcode)
	}
	return nil
}

// --- Manual pairing code -----------------------------------------------

// ManualCode renders the payload as an 11-digit pairing code, or 21 digits
// when the payload declares a custom commissioning flow (which requires the
// vendor and product ID to be present, per specification).
func (p Payload) ManualCode() (string, error) {
	if err := validatePasscode(p.Passcode); err != nil {
		return "", err
	}
	if p.Discriminator > 0xFFF {
		return "", fmt.Errorf("discriminator %d does not fit 12 bits", p.Discriminator)
	}
	shortDiscriminator := p.Discriminator >> 8
	includeVidPid := p.CustomFlow != 0
	vidPresent := uint16(0)
	if includeVidPid {
		vidPresent = 1
	}
	code := fmt.Sprintf("%d%05d%04d",
		vidPresent<<2|shortDiscriminator>>2,
		(uint32(shortDiscriminator&0x3)<<14)|(p.Passcode&0x3FFF),
		p.Passcode>>14)
	if includeVidPid {
		code += fmt.Sprintf("%05d%05d", p.VendorID, p.ProductID)
	}
	return code + string(rune('0'+verhoeffCheckDigit(code))), nil
}

// ParseManualCode decodes an 11- or 21-digit manual pairing code. Spaces,
// dashes and dots between digit groups are accepted.
func ParseManualCode(code string) (Payload, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '.' {
			return -1
		}
		return r
	}, code)
	if len(cleaned) != 11 && len(cleaned) != 21 {
		return Payload{}, fmt.Errorf("manual pairing code must have 11 or 21 digits, got %d", len(cleaned))
	}
	digits := make([]int, len(cleaned))
	for index, character := range cleaned {
		if character < '0' || character > '9' {
			return Payload{}, fmt.Errorf("manual pairing code contains %q", character)
		}
		digits[index] = int(character - '0')
	}
	if !verhoeffValidate(cleaned) {
		return Payload{}, errors.New("manual pairing code check digit is invalid")
	}
	first := digits[0]
	if first > 7 {
		return Payload{}, errors.New("manual pairing code uses a reserved leading digit")
	}
	group2 := digitsValue(cleaned[1:6])
	if group2 > 0xFFFF {
		// Only 2 bits of short discriminator ride above the 14 passcode
		// bits, so this chunk never legitimately exceeds 16 bits.
		return Payload{}, errors.New("manual pairing code digit group 2-6 is out of range")
	}
	group3 := digitsValue(cleaned[6:10])
	payload := Payload{
		Discriminator:      uint16(first&0x3)<<10 | uint16(group2>>14)<<8,
		ShortDiscriminator: true,
		Passcode:           group2&0x3FFF | group3<<14,
	}
	vidPresent := first>>2&1 == 1
	if vidPresent != (len(cleaned) == 21) {
		return Payload{}, fmt.Errorf("manual pairing code has %d digits but its leading digit declares vendor/product %v",
			len(cleaned), vidPresent)
	}
	if vidPresent {
		vendor := digitsValue(cleaned[10:15])
		product := digitsValue(cleaned[15:20])
		if vendor > 0xFFFF || product > 0xFFFF {
			return Payload{}, errors.New("manual pairing code vendor or product ID exceeds 65535")
		}
		payload.CustomFlow = 2
		payload.VendorID = uint16(vendor)
		payload.ProductID = uint16(product)
	}
	if err := validatePasscode(payload.Passcode); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func digitsValue(digits string) uint32 {
	var value uint32
	for _, character := range digits {
		value = value*10 + uint32(character-'0')
	}
	return value
}

// Verhoeff check digit, as required for manual pairing codes.
var verhoeffD = [10][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
	{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
	{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
	{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
	{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
	{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
	{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
	{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
	{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
}

var verhoeffP = [8][10]int{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
	{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
	{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
	{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
	{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
	{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
	{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
}

var verhoeffInverse = [10]int{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}

func verhoeffCheckDigit(digits string) int {
	checksum := 0
	for index := 0; index < len(digits); index++ {
		digit := int(digits[len(digits)-1-index] - '0')
		checksum = verhoeffD[checksum][verhoeffP[(index+1)%8][digit]]
	}
	return verhoeffInverse[checksum]
}

func verhoeffValidate(digitsWithCheck string) bool {
	checksum := 0
	for index := 0; index < len(digitsWithCheck); index++ {
		digit := int(digitsWithCheck[len(digitsWithCheck)-1-index] - '0')
		checksum = verhoeffD[checksum][verhoeffP[index%8][digit]]
	}
	return checksum == 0
}

// --- QR payload ---------------------------------------------------------

const base38Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ-."

const qrHeaderBytes = 11 // 88 bits of fixed fields

// QR renders the payload as "MT:" QR-code content. The full 12-bit
// discriminator is required, so a payload parsed from a manual code cannot
// be re-encoded as QR. A parsed optional TLV section is re-emitted verbatim.
func (p Payload) QR() (string, error) {
	if err := validatePasscode(p.Passcode); err != nil {
		return "", err
	}
	if p.ShortDiscriminator {
		return "", errors.New("QR payload needs the full 12-bit discriminator")
	}
	if p.Version != 0 {
		return "", fmt.Errorf("unsupported QR payload version %d", p.Version)
	}
	if p.CustomFlow > 2 {
		return "", fmt.Errorf("commissioning flow %d is reserved", p.CustomFlow)
	}
	if p.Discriminator > 0xFFF {
		return "", fmt.Errorf("discriminator %d does not fit 12 bits", p.Discriminator)
	}
	var bits bitWriter
	bits.write(uint32(p.Version), 3)
	bits.write(uint32(p.VendorID), 16)
	bits.write(uint32(p.ProductID), 16)
	bits.write(uint32(p.CustomFlow), 2)
	bits.write(uint32(p.Discovery), 8)
	bits.write(uint32(p.Discriminator), 12)
	bits.write(p.Passcode, 27)
	bits.write(0, 4) // padding to 88 bits
	return qrPrefix + base38Encode(append(bits.data, p.extensionBytes...)), nil
}

// ParseQR decodes "MT:" QR-code content. Bytes beyond the fixed 88-bit
// header are the optional TLV section (serial number, vendor-specific
// elements) and must decode as valid TLV.
func ParseQR(code string) (Payload, error) {
	trimmed := strings.TrimSpace(code)
	if !strings.HasPrefix(strings.ToUpper(trimmed), qrPrefix) {
		return Payload{}, errors.New("QR payload must start with MT:")
	}
	packed, err := base38Decode(trimmed[len(qrPrefix):])
	if err != nil {
		return Payload{}, err
	}
	if len(packed) < qrHeaderBytes {
		return Payload{}, fmt.Errorf("QR payload is %d bytes, want at least %d", len(packed), qrHeaderBytes)
	}
	bits := bitReader{data: packed}
	payload := Payload{
		Version:       uint8(bits.read(3)),
		VendorID:      uint16(bits.read(16)),
		ProductID:     uint16(bits.read(16)),
		CustomFlow:    uint8(bits.read(2)),
		Discovery:     uint8(bits.read(8)),
		Discriminator: uint16(bits.read(12)),
		Passcode:      bits.read(27),
	}
	if bits.read(4) != 0 {
		return Payload{}, errors.New("QR payload has non-zero padding")
	}
	if payload.Version != 0 {
		return Payload{}, fmt.Errorf("unsupported QR payload version %d", payload.Version)
	}
	if payload.CustomFlow > 2 {
		return Payload{}, fmt.Errorf("commissioning flow %d is reserved", payload.CustomFlow)
	}
	if err := validatePasscode(payload.Passcode); err != nil {
		return Payload{}, err
	}
	if extension := packed[qrHeaderBytes:]; len(extension) > 0 {
		decoded, dumpErr := tlv.Dump(extension)
		if dumpErr != nil {
			return Payload{}, fmt.Errorf("QR payload optional TLV section: %w", dumpErr)
		}
		payload.Extensions = decoded
		payload.extensionBytes = extension
	}
	return payload, nil
}

// The packed payload is a little-endian bit string: each field is appended
// least-significant bit first.
type bitWriter struct {
	data []byte
	used int
}

func (w *bitWriter) write(value uint32, count int) {
	for index := 0; index < count; index++ {
		if w.used%8 == 0 {
			w.data = append(w.data, 0)
		}
		if value>>index&1 == 1 {
			w.data[w.used/8] |= 1 << (w.used % 8)
		}
		w.used++
	}
}

type bitReader struct {
	data []byte
	used int
}

func (r *bitReader) read(count int) uint32 {
	var value uint32
	for index := 0; index < count; index++ {
		if r.data[r.used/8]>>(r.used%8)&1 == 1 {
			value |= 1 << index
		}
		r.used++
	}
	return value
}

// Base38 groups 3 bytes into 5 characters (trailing 2 bytes into 4, a single
// byte into 2), each group little-endian.
func base38Encode(data []byte) string {
	var result strings.Builder
	for offset := 0; offset < len(data); offset += 3 {
		remaining := len(data) - offset
		var value uint32
		characters := 0
		switch {
		case remaining >= 3:
			value = uint32(data[offset]) | uint32(data[offset+1])<<8 | uint32(data[offset+2])<<16
			characters = 5
		case remaining == 2:
			value = uint32(data[offset]) | uint32(data[offset+1])<<8
			characters = 4
		default:
			value = uint32(data[offset])
			characters = 2
		}
		for index := 0; index < characters; index++ {
			result.WriteByte(base38Alphabet[value%38])
			value /= 38
		}
	}
	return result.String()
}

func base38Decode(text string) ([]byte, error) {
	var result []byte
	for offset := 0; offset < len(text); offset += 5 {
		remaining := len(text) - offset
		var characters, bytes int
		switch {
		case remaining >= 5:
			characters, bytes = 5, 3
		case remaining == 4:
			characters, bytes = 4, 2
		case remaining == 2:
			characters, bytes = 2, 1
		default:
			return nil, fmt.Errorf("base38 text has invalid trailing length %d", remaining)
		}
		var value uint64
		for index := characters - 1; index >= 0; index-- {
			position := strings.IndexByte(base38Alphabet, text[offset+index])
			if position < 0 {
				return nil, fmt.Errorf("invalid base38 character %q", text[offset+index])
			}
			value = value*38 + uint64(position)
		}
		if value >= 1<<(8*bytes) {
			return nil, errors.New("base38 group overflows its byte width")
		}
		for index := 0; index < bytes; index++ {
			result = append(result, byte(value>>(8*index)))
		}
	}
	return result, nil
}
