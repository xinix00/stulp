package tlv

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Encoding vectors from the Matter Core Specification TLV appendix.
func TestSpecificationVectors(t *testing.T) {
	cases := []struct {
		name  string
		build func(w *Writer)
		want  []byte
	}{
		{"unsigned 42 anonymous", func(w *Writer) { w.PutUint(Anonymous(), 42) }, []byte{0x04, 0x2A}},
		{"signed -17 anonymous", func(w *Writer) { w.PutInt(Anonymous(), -17) }, []byte{0x00, 0xEF}},
		{"boolean false", func(w *Writer) { w.PutBool(Anonymous(), false) }, []byte{0x08}},
		{"boolean true", func(w *Writer) { w.PutBool(Anonymous(), true) }, []byte{0x09}},
		{"utf-8 Hello!", func(w *Writer) { w.PutString(Anonymous(), "Hello!") },
			[]byte{0x0C, 0x06, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x21}},
		{"octet string", func(w *Writer) { w.PutBytes(Anonymous(), []byte{0, 1, 2, 3, 4}) },
			[]byte{0x10, 0x05, 0x00, 0x01, 0x02, 0x03, 0x04}},
		{"null", func(w *Writer) { w.PutNull(Anonymous()) }, []byte{0x14}},
		{"float32 17.9", func(w *Writer) { w.PutFloat32(Anonymous(), 17.9) },
			[]byte{0x0A, 0x33, 0x33, 0x8F, 0x41}},
		{"float64 17.9", func(w *Writer) { w.PutFloat64(Anonymous(), 17.9) },
			[]byte{0x0B, 0x66, 0x66, 0x66, 0x66, 0x66, 0xE6, 0x31, 0x40}},
		{"empty structure", func(w *Writer) { w.StartStructure(Anonymous()); w.EndContainer() },
			[]byte{0x15, 0x18}},
		{"context tag 1 unsigned 42", func(w *Writer) { w.PutUint(Context(1), 42) },
			[]byte{0x24, 0x01, 0x2A}},
		{"structure {0:42, 1:-17}", func(w *Writer) {
			w.StartStructure(Anonymous())
			w.PutUint(Context(0), 42)
			w.PutInt(Context(1), -17)
			w.EndContainer()
		}, []byte{0x15, 0x24, 0x00, 0x2A, 0x20, 0x01, 0xEF, 0x18}},
		{"array of signed 0..4", func(w *Writer) {
			w.StartArray(Anonymous())
			for value := int64(0); value < 5; value++ {
				w.PutInt(Anonymous(), value)
			}
			w.EndContainer()
		}, []byte{0x16, 0x00, 0x00, 0x00, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x18}},
		{"unsigned 65535 uses two octets", func(w *Writer) { w.PutUint(Anonymous(), 0xFFFF) },
			[]byte{0x05, 0xFF, 0xFF}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var writer Writer
			testCase.build(&writer)
			got, err := writer.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, testCase.want) {
				t.Fatalf("encoded % X, want % X", got, testCase.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	var writer Writer
	writer.StartStructure(Anonymous())
	writer.PutUint(Context(0), math.MaxUint64)
	writer.PutInt(Context(1), math.MinInt64)
	writer.PutString(Context(2), "hopy")
	writer.PutBytes(Context(3), bytes.Repeat([]byte{0xAB}, 300)) // 2-octet length
	writer.PutFloat64(Context(4), -0.25)
	writer.PutNull(Context(5))
	writer.StartArray(Context(6))
	writer.PutBool(Anonymous(), true)
	writer.EndContainer()
	writer.PutUint(Common(0x100), 7)
	writer.PutUint(Full(0xFFF1, 0xDEED, 0x10000), 8)
	writer.EndContainer()
	data, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	reader := NewReader(data)
	var elements []Element
	for {
		element, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		elements = append(elements, element)
	}
	if len(elements) != 13 {
		t.Fatalf("got %d elements, want 13", len(elements))
	}
	if elements[1].Uint != math.MaxUint64 || elements[2].Int != math.MinInt64 {
		t.Fatalf("extreme integers did not round-trip: %#v %#v", elements[1], elements[2])
	}
	if string(elements[3].Data) != "hopy" || len(elements[4].Data) != 300 {
		t.Fatalf("strings did not round-trip")
	}
	if elements[5].Float != -0.25 || elements[6].Type != TypeNull {
		t.Fatalf("float/null did not round-trip")
	}
	if elements[10].Tag != Common(0x100) || elements[11].Tag != Full(0xFFF1, 0xDEED, 0x10000) {
		t.Fatalf("profile tags did not round-trip: %v %v", elements[10].Tag, elements[11].Tag)
	}
}

func TestDump(t *testing.T) {
	var writer Writer
	writer.StartStructure(Anonymous())
	writer.PutUint(Context(0), 42)
	writer.StartArray(Context(1))
	writer.PutString(Anonymous(), "a")
	writer.PutString(Anonymous(), "b")
	writer.EndContainer()
	writer.PutBytes(Context(2), []byte{0xBE, 0xEF})
	writer.EndContainer()
	data, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	value, err := Dump(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"0": uint64(42),
		"1": []any{"a", "b"},
		"2": "0xbeef",
	}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("Dump = %#v, want %#v", value, want)
	}
}

func TestPutUintWidthPreservesMatterCertificateWidths(t *testing.T) {
	var writer Writer
	writer.PutUintWidth(Context(1), 1, 1)
	writer.PutUintWidth(Context(2), 1, 2)
	writer.PutUintWidth(Context(3), 1, 4)
	writer.PutUintWidth(Context(4), 1, 8)
	encoded, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x24, 0x01, 0x01,
		0x25, 0x02, 0x01, 0x00,
		0x26, 0x03, 0x01, 0x00, 0x00, 0x00,
		0x27, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("explicit-width uint = %x, want %x", encoded, want)
	}
}

func TestDepthLimit(t *testing.T) {
	// MaxDepth opens must work; one more must fail with a depth error.
	deepest := bytes.Repeat([]byte{0x15}, MaxDepth)
	reader := NewReader(deepest)
	for range MaxDepth {
		if _, err := reader.Next(); err != nil {
			t.Fatalf("open within MaxDepth failed: %v", err)
		}
	}

	bomb := bytes.Repeat([]byte{0x15}, MaxDepth+1)
	reader = NewReader(bomb)
	var err error
	for err == nil {
		_, err = reader.Next()
	}
	if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "nest deeper") {
		t.Fatalf("depth bomb error = %v", err)
	}
	if _, err := Dump(bomb); err == nil {
		t.Fatal("Dump must reject the depth bomb")
	}
}

func TestMalformedInput(t *testing.T) {
	malformed := [][]byte{
		{0x15},             // unterminated structure
		{0x18},             // end without container
		{0x04},             // integer without payload
		{0x0C, 0x05, 0x41}, // string shorter than its length
		{0x1F},             // reserved element type
	}
	for _, data := range malformed {
		reader := NewReader(data)
		var err error
		for err == nil {
			_, err = reader.Next()
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("input % X decoded without error", data)
		}
	}
}
