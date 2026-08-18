package onboarding

import (
	"strings"
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// The canonical Matter test device: passcode 20202021, discriminator 3840
// (0xF00), vendor 0xFFF1. Its manual pairing code is 34970112332 and its QR
// payload is MT:-24J0AFN00KA0648G00 (as printed by chip-tool and matter.js).
const (
	testPasscode      = 20202021
	testDiscriminator = 3840
	testManualCode    = "34970112332"
	testQR            = "MT:-24J0AFN00KA0648G00"
)

func TestVerhoeff(t *testing.T) {
	if digit := verhoeffCheckDigit("236"); digit != 3 {
		t.Fatalf("check digit of 236 = %d, want 3", digit)
	}
	if !verhoeffValidate("2363") {
		t.Fatal("2363 must validate")
	}
	if verhoeffValidate("2364") {
		t.Fatal("2364 must not validate")
	}
}

func TestManualCodeRoundTrip(t *testing.T) {
	payload := Payload{Discriminator: testDiscriminator, Passcode: testPasscode}
	code, err := payload.ManualCode()
	if err != nil {
		t.Fatal(err)
	}
	if code != testManualCode {
		t.Fatalf("manual code = %s, want %s", code, testManualCode)
	}

	parsed, err := ParseManualCode("3497-011-2332") // separators allowed
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Passcode != testPasscode {
		t.Fatalf("passcode = %d, want %d", parsed.Passcode, testPasscode)
	}
	if !parsed.ShortDiscriminator || parsed.Discriminator != testDiscriminator&0xF00 {
		t.Fatalf("discriminator = %#v", parsed)
	}
}

func TestManualCodeWithVendorProduct(t *testing.T) {
	payload := Payload{
		Discriminator: testDiscriminator, Passcode: testPasscode,
		CustomFlow: 2, VendorID: 65521, ProductID: 32768,
	}
	code, err := payload.ManualCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 21 {
		t.Fatalf("code %s has %d digits, want 21", code, len(code))
	}
	parsed, err := ParseManualCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.VendorID != 65521 || parsed.ProductID != 32768 || parsed.Passcode != testPasscode {
		t.Fatalf("parsed %+v", parsed)
	}
}

func TestManualCodeRejectsCorruption(t *testing.T) {
	if _, err := ParseManualCode("34970112333"); err == nil {
		t.Fatal("corrupted check digit must be rejected")
	}
	if _, err := ParseManualCode("123"); err == nil {
		t.Fatal("wrong length must be rejected")
	}
}

// withCheck completes a digit string with a valid Verhoeff check digit, so
// these tests exercise the semantic validation rather than the checksum.
func withCheck(digits string) string {
	return digits + string(rune('0'+verhoeffCheckDigit(digits)))
}

func TestManualCodeRejectsInvalidCombinations(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"21 digits without the vendor/product flag", withCheck("3497011233" + "0000100002")},
		{"11 digits with the vendor/product flag", withCheck("7497011233")},
		{"digit group 2-6 out of range", withCheck("0" + "99999" + "1233")},
		{"vendor ID above 65535", withCheck("7497011233" + "99999" + "00001")},
		{"product ID above 65535", withCheck("7497011233" + "00001" + "99999")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if payload, err := ParseManualCode(testCase.code); err == nil {
				t.Fatalf("code %s was accepted: %+v", testCase.code, payload)
			}
		})
	}
}

func TestManualCodeRejectsWideDiscriminator(t *testing.T) {
	payload := Payload{Discriminator: 0x1000, Passcode: testPasscode}
	if _, err := payload.ManualCode(); err == nil {
		t.Fatal("13-bit discriminator must be rejected, not truncated")
	}
}

func TestQRDecodeCanonicalPayload(t *testing.T) {
	payload, err := ParseQR(testQR)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Passcode != testPasscode {
		t.Fatalf("passcode = %d, want %d", payload.Passcode, testPasscode)
	}
	if payload.Discriminator != testDiscriminator {
		t.Fatalf("discriminator = %d, want %d", payload.Discriminator, testDiscriminator)
	}
	if payload.VendorID != 65521 {
		t.Fatalf("vendor = %d, want 65521", payload.VendorID)
	}

	// Re-encoding must reproduce the exact original payload text.
	encoded, err := payload.QR()
	if err != nil {
		t.Fatal(err)
	}
	if encoded != testQR {
		t.Fatalf("re-encoded QR = %s, want %s", encoded, testQR)
	}

	// The same payload must yield the canonical manual code.
	manual, err := payload.ManualCode()
	if err != nil {
		t.Fatal(err)
	}
	if manual != testManualCode {
		t.Fatalf("manual code from QR payload = %s, want %s", manual, testManualCode)
	}
}

func TestQRRejectsOutOfRangeFields(t *testing.T) {
	base := Payload{Discriminator: testDiscriminator, Passcode: testPasscode}
	cases := []struct {
		name   string
		mutate func(*Payload)
	}{
		{"version", func(p *Payload) { p.Version = 1 }},
		{"reserved commissioning flow", func(p *Payload) { p.CustomFlow = 3 }},
		{"13-bit discriminator", func(p *Payload) { p.Discriminator = 0x1000 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := base
			testCase.mutate(&payload)
			if code, err := payload.QR(); err == nil {
				t.Fatalf("payload %+v encoded as %s instead of failing", payload, code)
			}
		})
	}
}

func TestQRExtensionRoundTrip(t *testing.T) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutString(tlv.Context(0), "SN-1234")
	writer.EndContainer()
	extension, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	payload := Payload{
		VendorID: 65521, ProductID: 32768,
		Discriminator: testDiscriminator, Passcode: testPasscode,
		extensionBytes: extension,
	}
	code, err := payload.QR()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseQR(code)
	if err != nil {
		t.Fatalf("QR with optional TLV section was rejected: %v", err)
	}
	if parsed.Passcode != testPasscode || parsed.Discriminator != testDiscriminator {
		t.Fatalf("fixed fields corrupted by extension: %+v", parsed)
	}
	extensions, ok := parsed.Extensions.(map[string]any)
	if !ok || extensions["0"] != "SN-1234" {
		t.Fatalf("Extensions = %#v, want serial number under tag 0", parsed.Extensions)
	}
	again, err := parsed.QR()
	if err != nil {
		t.Fatal(err)
	}
	if again != code {
		t.Fatalf("extension did not round-trip: %s != %s", again, code)
	}
}

func TestQRRejectsMalformedExtension(t *testing.T) {
	packed, err := base38Decode(testQR[3:])
	if err != nil {
		t.Fatal(err)
	}
	corrupted := "MT:" + base38Encode(append(packed, 0x1F)) // reserved TLV type
	if payload, err := ParseQR(corrupted); err == nil {
		t.Fatalf("malformed extension TLV was accepted: %+v", payload)
	}
}

func TestParseDispatch(t *testing.T) {
	if _, err := Parse(" mt:" + testQR[3:]); err != nil {
		t.Fatalf("lowercase MT: prefix: %v", err)
	}
	if _, err := Parse(testManualCode); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidPasscodes(t *testing.T) {
	for _, passcode := range []uint32{0, 11111111, 12345678, 1 << 27} {
		payload := Payload{Discriminator: 1, Passcode: passcode}
		if _, err := payload.QR(); err == nil {
			t.Fatalf("passcode %d must be rejected", passcode)
		}
	}
}

func TestBase38(t *testing.T) {
	for _, data := range [][]byte{{0x00}, {0xFF}, {0x01, 0x02}, {0xDE, 0xAD, 0xBE}, {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}} {
		decoded, err := base38Decode(base38Encode(data))
		if err != nil {
			t.Fatal(err)
		}
		if string(decoded) != string(data) {
			t.Fatalf("round trip of % X gave % X", data, decoded)
		}
	}
	if _, err := base38Decode("ABC"); err == nil {
		t.Fatal("length 3 must be rejected")
	}
	if _, err := base38Decode(strings.Repeat("$", 5)); err == nil {
		t.Fatal("invalid characters must be rejected")
	}
}
