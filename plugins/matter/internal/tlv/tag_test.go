package tlv

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
)

// Every tag form has its own control byte and width. They all come off the
// network, so each decode path needs exercising.
func TestAllTagFormsRoundTrip(t *testing.T) {
	tags := []Tag{
		Anonymous(),
		Context(0),
		Context(255),
		Common(0),
		Common(0xFFFF),      // 16-bit common form
		Common(0x10000),     // 32-bit common form
		Implicit(1),         // 16-bit implicit form
		Implicit(0x20000),   // 32-bit implicit form
		Full(0xFFF1, 0, 1),  // 48-bit fully qualified form
		Full(1, 2, 0x30000), // 64-bit fully qualified form
	}
	var writer Writer
	writer.StartStructure(Anonymous())
	for index, tag := range tags {
		writer.PutUint(tag, uint64(index))
	}
	writer.EndContainer()
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	reader := NewReader(encoded)
	if element, err := reader.Next(); err != nil || element.Type != TypeStructure {
		t.Fatalf("expected the opening structure: %+v %v", element, err)
	}
	for index, want := range tags {
		element, err := reader.Next()
		if err != nil {
			t.Fatalf("tag %v: %v", want, err)
		}
		if element.Tag != want {
			t.Fatalf("tag %d decoded as %v, want %v", index, element.Tag, want)
		}
		if element.Uint != uint64(index) {
			t.Fatalf("tag %v carried %d, want %d", want, element.Uint, index)
		}
	}
}

func TestContextNumber(t *testing.T) {
	if number, ok := Context(7).ContextNumber(); !ok || number != 7 {
		t.Fatalf("ContextNumber = %d, %v", number, ok)
	}
	for _, tag := range []Tag{Anonymous(), Common(1), Implicit(1), Full(1, 2, 3)} {
		if _, ok := tag.ContextNumber(); ok {
			t.Fatalf("%v reported itself as a context tag", tag)
		}
	}
}

func TestTagStrings(t *testing.T) {
	cases := map[string]Tag{
		"anonymous":   Anonymous(),
		"7":           Context(7),
		"common:9":    Common(9),
		"implicit:9":  Implicit(9),
		"fff1/0002:3": Full(0xFFF1, 2, 3),
	}
	for want, tag := range cases {
		if got := tag.String(); got != want {
			t.Fatalf("tag string %q, want %q", got, want)
		}
	}
}

// Signed integers are stored in the narrowest width that fits, so each
// boundary needs to survive sign extension on the way back.
func TestSignedIntegerWidths(t *testing.T) {
	values := []int64{
		0, -1, 1,
		math.MinInt8, math.MaxInt8, math.MinInt8 - 1, math.MaxInt8 + 1,
		math.MinInt16, math.MaxInt16, math.MinInt16 - 1, math.MaxInt16 + 1,
		math.MinInt32, math.MaxInt32, math.MinInt32 - 1, math.MaxInt32 + 1,
		math.MinInt64, math.MaxInt64,
	}
	for _, value := range values {
		var writer Writer
		writer.PutInt(Anonymous(), value)
		encoded, err := writer.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		element, err := NewReader(encoded).Next()
		if err != nil {
			t.Fatalf("%d: %v", value, err)
		}
		if element.Type != TypeInt || element.Int != value {
			t.Fatalf("%d round-tripped as %d (type %d)", value, element.Int, element.Type)
		}
	}
}

func TestUnsignedIntegerWidths(t *testing.T) {
	for _, value := range []uint64{0, 0xFF, 0x100, 0xFFFF, 0x10000, 0xFFFFFFFF, 0x100000000, math.MaxUint64} {
		var writer Writer
		writer.PutUint(Anonymous(), value)
		encoded, err := writer.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		element, err := NewReader(encoded).Next()
		if err != nil {
			t.Fatalf("%d: %v", value, err)
		}
		if element.Type != TypeUint || element.Uint != value {
			t.Fatalf("%d round-tripped as %d", value, element.Uint)
		}
	}
}

func TestListContainer(t *testing.T) {
	var writer Writer
	writer.StartList(Context(3))
	writer.PutUint(Anonymous(), 1)
	writer.PutString(Context(1), "in a list")
	writer.EndContainer()
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	value, err := Dump(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, []any{uint64(1), "in a list"}) {
		t.Fatalf("list dumped as %#v", value)
	}
}

func TestWriterErrorPaths(t *testing.T) {
	t.Run("unterminated container", func(t *testing.T) {
		var writer Writer
		writer.StartStructure(Anonymous())
		writer.PutUint(Context(1), 1)
		if _, err := writer.Bytes(); err == nil {
			t.Fatal("an unterminated container was returned as valid TLV")
		}
	})

	t.Run("end without a container", func(t *testing.T) {
		var writer Writer
		writer.EndContainer()
		if _, err := writer.Bytes(); err == nil {
			t.Fatal("a stray EndContainer was accepted")
		}
	})

	t.Run("context tag beyond one octet", func(t *testing.T) {
		var writer Writer
		writer.control(Tag{kind: tagContext, number: 0x100}, wireUint)
		if _, err := writer.Bytes(); err == nil {
			t.Fatal("an oversized context tag was encoded")
		}
	})

	t.Run("the first error sticks", func(t *testing.T) {
		var writer Writer
		writer.EndContainer() // records the first failure
		writer.PutUint(Anonymous(), 1)
		writer.EndContainer()
		_, err := writer.Bytes()
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("EndContainer")) {
			t.Fatalf("the first error did not survive: %v", err)
		}
	})
}

func TestReaderRejectsReservedTagControl(t *testing.T) {
	// Tag control 0xE0 is the 64-bit fully qualified form; a truncated one
	// must fail rather than read past the buffer.
	if _, err := NewReader([]byte{0xE0 | wireUint, 0x01, 0x02}).Next(); err == nil {
		t.Fatal("a truncated fully qualified tag was accepted")
	}
	for _, control := range []byte{0x40, 0x60, 0x80, 0xA0, 0xC0} {
		if _, err := NewReader([]byte{control | wireUint}).Next(); err == nil {
			t.Fatalf("a truncated tag with control %#x was accepted", control)
		}
	}
}

func TestDumpTopLevelSequence(t *testing.T) {
	var writer Writer
	writer.PutUint(Anonymous(), 1)
	writer.PutString(Anonymous(), "two")
	writer.PutNull(Anonymous())
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	value, err := Dump(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, []any{uint64(1), "two", nil}) {
		t.Fatalf("dumped as %#v", value)
	}
}

func TestDumpFloatAndBool(t *testing.T) {
	var writer Writer
	writer.StartStructure(Anonymous())
	writer.PutFloat32(Context(1), 1.5)
	writer.PutFloat64(Context(2), -2.25)
	writer.PutBool(Context(3), true)
	writer.EndContainer()
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	value, err := Dump(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"1": 1.5, "2": -2.25, "3": true}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("dumped as %#v, want %#v", value, want)
	}
}

func TestReaderReportsEndOfInput(t *testing.T) {
	reader := NewReader(nil)
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty input gave %v, want io.EOF", err)
	}
}
