package commissioning

import (
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func TestNewWindowParametersProducesValidFreshCredentials(t *testing.T) {
	first, err := NewWindowParameters(15 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWindowParameters(15 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Timeout != 900 || first.Passcode == 0 || forbiddenSetupPasscodes[first.Passcode] ||
		first.Discriminator > 0xFFF || first.Iterations != defaultWindowIterations ||
		len(first.Salt) != 32 || len(first.Verifier) != 97 {
		t.Fatalf("invalid parameters: %#v", first)
	}
	if first.Passcode == second.Passcode || string(first.Salt) == string(second.Salt) || string(first.Verifier) == string(second.Verifier) {
		t.Fatal("two commissioning windows reused credentials")
	}
}

func TestOpenWindowCommandCarriesMatterAdministratorFields(t *testing.T) {
	parameters := WindowParameters{
		Timeout: 900, Passcode: 34567890, Discriminator: 0xABC, Iterations: 10000,
		Salt: make([]byte, 32), Verifier: make([]byte, 97),
	}
	command, err := openWindowCommand(0, parameters)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := im.EncodeInvokeRequest([]im.Command{command}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := im.DecodeInvokeRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Path != command.Path {
		t.Fatalf("decoded command = %#v", decoded)
	}
	fields := decoded[0].Fields
	assertUintField(t, fields, 0, 900)
	assertBytesField(t, fields, 1, 97)
	assertUintField(t, fields, 2, 0xABC)
	assertUintField(t, fields, 3, 10000)
	assertBytesField(t, fields, 4, 32)
}

func assertUintField(t *testing.T, fields im.Value, tag uint8, want uint64) {
	t.Helper()
	field, ok := fields.Field(tag)
	if !ok || field.Type != tlv.TypeUint || field.Uint != want {
		t.Fatalf("field %d = %#v, want uint %d", tag, field, want)
	}
}

func assertBytesField(t *testing.T, fields im.Value, tag uint8, want int) {
	t.Helper()
	field, ok := fields.Field(tag)
	if !ok || field.Type != tlv.TypeBytes || len(field.Data) != want {
		t.Fatalf("field %d = %#v, want %d bytes", tag, field, want)
	}
}
