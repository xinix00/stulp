// Package tlv implements Matter TLV (tag-length-value) encoding as defined in
// the Matter Core Specification, appendix "Tag-length-value (TLV) encoding
// format". Every Matter payload above the message layer (interaction model,
// commissioning, operational certificates) is TLV, so this codec is the
// foundation of the Matter stack.
package tlv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Type is the semantic type of a decoded element.
type Type uint8

const (
	TypeInt Type = iota
	TypeUint
	TypeBool
	TypeFloat
	TypeString
	TypeBytes
	TypeNull
	TypeStructure
	TypeArray
	TypeList
	TypeEnd
)

// Wire element types (low 5 bits of the control byte).
const (
	wireInt       = 0x00 // +0..3 width log2
	wireUint      = 0x04 // +0..3 width log2
	wireBoolFalse = 0x08
	wireBoolTrue  = 0x09
	wireFloat32   = 0x0A
	wireFloat64   = 0x0B
	wireUTF8      = 0x0C // +0..3 length-width log2
	wireBytes     = 0x10 // +0..3 length-width log2
	wireNull      = 0x14
	wireStructure = 0x15
	wireArray     = 0x16
	wireList      = 0x17
	wireEnd       = 0x18
)

// Tag control values (high 3 bits of the control byte).
const (
	tagCtrlAnonymous  = 0x00
	tagCtrlContext    = 0x20
	tagCtrlCommon16   = 0x40
	tagCtrlCommon32   = 0x60
	tagCtrlImplicit16 = 0x80
	tagCtrlImplicit32 = 0xA0
	tagCtrlFull48     = 0xC0
	tagCtrlFull64     = 0xE0
)

type tagKind uint8

const (
	tagAnonymous tagKind = iota
	tagContext
	tagCommon
	tagImplicit
	tagFull
)

// Tag identifies a TLV element within its container.
type Tag struct {
	kind    tagKind
	vendor  uint16
	profile uint16
	number  uint32
}

func Anonymous() Tag             { return Tag{kind: tagAnonymous} }
func Context(number uint8) Tag   { return Tag{kind: tagContext, number: uint32(number)} }
func Common(number uint32) Tag   { return Tag{kind: tagCommon, number: number} }
func Implicit(number uint32) Tag { return Tag{kind: tagImplicit, number: number} }
func Full(vendor, profile uint16, number uint32) Tag {
	return Tag{kind: tagFull, vendor: vendor, profile: profile, number: number}
}

// ContextNumber reports the tag number when the tag is a context tag.
func (t Tag) ContextNumber() (uint8, bool) {
	if t.kind != tagContext {
		return 0, false
	}
	return uint8(t.number), true
}

func (t Tag) String() string {
	switch t.kind {
	case tagAnonymous:
		return "anonymous"
	case tagContext:
		return strconv.FormatUint(uint64(t.number), 10)
	case tagCommon:
		return fmt.Sprintf("common:%d", t.number)
	case tagImplicit:
		return fmt.Sprintf("implicit:%d", t.number)
	default:
		return fmt.Sprintf("%04x/%04x:%d", t.vendor, t.profile, t.number)
	}
}

// Element is one decoded TLV element. Type declares which value field is set.
type Element struct {
	Tag  Tag
	Type Type

	Int   int64
	Uint  uint64
	Bool  bool
	Float float64
	Data  []byte
}

// Writer builds a TLV byte sequence. Methods keep appending; the first error
// sticks and is reported by Bytes.
type Writer struct {
	buffer []byte
	depth  int
	err    error
}

func (w *Writer) Bytes() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.depth != 0 {
		return nil, fmt.Errorf("%d unterminated TLV container(s)", w.depth)
	}
	return w.buffer, nil
}

func (w *Writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

func (w *Writer) control(tag Tag, elementType byte) {
	switch tag.kind {
	case tagAnonymous:
		w.buffer = append(w.buffer, tagCtrlAnonymous|elementType)
	case tagContext:
		if tag.number > 0xFF {
			w.fail(fmt.Errorf("context tag %d does not fit one octet", tag.number))
			return
		}
		w.buffer = append(w.buffer, tagCtrlContext|elementType, byte(tag.number))
	case tagCommon, tagImplicit:
		short, long := byte(tagCtrlCommon16), byte(tagCtrlCommon32)
		if tag.kind == tagImplicit {
			short, long = tagCtrlImplicit16, tagCtrlImplicit32
		}
		if tag.number <= 0xFFFF {
			w.buffer = append(w.buffer, short|elementType)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, uint16(tag.number))
		} else {
			w.buffer = append(w.buffer, long|elementType)
			w.buffer = binary.LittleEndian.AppendUint32(w.buffer, tag.number)
		}
	case tagFull:
		if tag.number <= 0xFFFF {
			w.buffer = append(w.buffer, tagCtrlFull48|elementType)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, tag.vendor)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, tag.profile)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, uint16(tag.number))
		} else {
			w.buffer = append(w.buffer, tagCtrlFull64|elementType)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, tag.vendor)
			w.buffer = binary.LittleEndian.AppendUint16(w.buffer, tag.profile)
			w.buffer = binary.LittleEndian.AppendUint32(w.buffer, tag.number)
		}
	}
}

func uintWidthLog(value uint64) byte {
	switch {
	case value <= 0xFF:
		return 0
	case value <= 0xFFFF:
		return 1
	case value <= 0xFFFFFFFF:
		return 2
	default:
		return 3
	}
}

func intWidthLog(value int64) byte {
	switch {
	case value >= math.MinInt8 && value <= math.MaxInt8:
		return 0
	case value >= math.MinInt16 && value <= math.MaxInt16:
		return 1
	case value >= math.MinInt32 && value <= math.MaxInt32:
		return 2
	default:
		return 3
	}
}

func appendLE(buffer []byte, value uint64, widthLog byte) []byte {
	switch widthLog {
	case 0:
		return append(buffer, byte(value))
	case 1:
		return binary.LittleEndian.AppendUint16(buffer, uint16(value))
	case 2:
		return binary.LittleEndian.AppendUint32(buffer, uint32(value))
	default:
		return binary.LittleEndian.AppendUint64(buffer, value)
	}
}

func (w *Writer) PutUint(tag Tag, value uint64) {
	width := uintWidthLog(value)
	w.control(tag, wireUint+width)
	w.buffer = appendLE(w.buffer, value, width)
}

// PutUintWidth writes an unsigned integer with an explicit wire width. Most
// application TLV uses PutUint's canonical smallest width; Matter
// certificates deliberately preserve the ASN.1 field width (notably 64-bit
// Node/Fabric IDs and 32-bit validity times).
func (w *Writer) PutUintWidth(tag Tag, value uint64, bytes int) {
	var width byte
	switch bytes {
	case 1:
		width = 0
	case 2:
		width = 1
	case 4:
		width = 2
	case 8:
		width = 3
	default:
		w.fail(fmt.Errorf("unsigned TLV width must be 1, 2, 4 or 8 bytes, got %d", bytes))
		return
	}
	if width < 3 && value >= uint64(1)<<(bytes*8) {
		w.fail(fmt.Errorf("unsigned value %d does not fit %d TLV bytes", value, bytes))
		return
	}
	w.control(tag, wireUint+width)
	w.buffer = appendLE(w.buffer, value, width)
}

func (w *Writer) PutInt(tag Tag, value int64) {
	width := intWidthLog(value)
	w.control(tag, wireInt+width)
	w.buffer = appendLE(w.buffer, uint64(value), width)
}

func (w *Writer) PutBool(tag Tag, value bool) {
	if value {
		w.control(tag, wireBoolTrue)
	} else {
		w.control(tag, wireBoolFalse)
	}
}

func (w *Writer) PutFloat32(tag Tag, value float32) {
	w.control(tag, wireFloat32)
	w.buffer = binary.LittleEndian.AppendUint32(w.buffer, math.Float32bits(value))
}

func (w *Writer) PutFloat64(tag Tag, value float64) {
	w.control(tag, wireFloat64)
	w.buffer = binary.LittleEndian.AppendUint64(w.buffer, math.Float64bits(value))
}

func (w *Writer) PutString(tag Tag, value string) {
	w.putLengthPrefixed(tag, wireUTF8, []byte(value))
}

func (w *Writer) PutBytes(tag Tag, value []byte) {
	w.putLengthPrefixed(tag, wireBytes, value)
}

func (w *Writer) putLengthPrefixed(tag Tag, base byte, value []byte) {
	width := uintWidthLog(uint64(len(value)))
	w.control(tag, base+width)
	w.buffer = appendLE(w.buffer, uint64(len(value)), width)
	w.buffer = append(w.buffer, value...)
}

func (w *Writer) PutNull(tag Tag) { w.control(tag, wireNull) }

func (w *Writer) StartStructure(tag Tag) { w.control(tag, wireStructure); w.depth++ }
func (w *Writer) StartArray(tag Tag)     { w.control(tag, wireArray); w.depth++ }
func (w *Writer) StartList(tag Tag)      { w.control(tag, wireList); w.depth++ }

func (w *Writer) EndContainer() {
	if w.depth == 0 {
		w.fail(errors.New("EndContainer without an open container"))
		return
	}
	w.depth--
	w.buffer = append(w.buffer, wireEnd)
}

// MaxDepth bounds container nesting when reading. TLV arrives from the
// network, and unlimited nesting would let a tiny hostile payload drive
// unbounded recursion in consumers such as Dump. Real Matter payloads stay
// in single digits.
const MaxDepth = 32

// Reader streams elements from a TLV byte sequence. Container elements are
// returned as-is; the caller sees their children next, terminated by a
// TypeEnd element. Memory use is bounded by the input: length prefixes are
// validated against the remaining data before any allocation, and nesting is
// capped at MaxDepth.
type Reader struct {
	data  []byte
	pos   int
	depth int
}

func NewReader(data []byte) *Reader { return &Reader{data: data} }

// Next returns the next element, or io.EOF after the final element of a
// well-formed sequence.
func (r *Reader) Next() (Element, error) {
	if r.pos >= len(r.data) {
		if r.depth != 0 {
			return Element{}, fmt.Errorf("TLV ends with %d unterminated container(s)", r.depth)
		}
		return Element{}, io.EOF
	}
	control := r.data[r.pos]
	r.pos++
	elementType := control & 0x1F
	tag, err := r.readTag(control & 0xE0)
	if err != nil {
		return Element{}, err
	}
	element := Element{Tag: tag}
	switch {
	case elementType >= wireInt && elementType < wireInt+4:
		raw, err := r.readLE(elementType - wireInt)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeInt
		element.Int = signExtend(raw, elementType-wireInt)
	case elementType >= wireUint && elementType < wireUint+4:
		raw, err := r.readLE(elementType - wireUint)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeUint
		element.Uint = raw
	case elementType == wireBoolFalse, elementType == wireBoolTrue:
		element.Type = TypeBool
		element.Bool = elementType == wireBoolTrue
	case elementType == wireFloat32:
		raw, err := r.readLE(2)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeFloat
		element.Float = float64(math.Float32frombits(uint32(raw)))
	case elementType == wireFloat64:
		raw, err := r.readLE(3)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeFloat
		element.Float = math.Float64frombits(raw)
	case elementType >= wireUTF8 && elementType < wireUTF8+4:
		data, err := r.readLengthPrefixed(elementType - wireUTF8)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeString
		element.Data = data
	case elementType >= wireBytes && elementType < wireBytes+4:
		data, err := r.readLengthPrefixed(elementType - wireBytes)
		if err != nil {
			return Element{}, err
		}
		element.Type = TypeBytes
		element.Data = data
	case elementType == wireNull:
		element.Type = TypeNull
	case elementType == wireStructure, elementType == wireArray, elementType == wireList:
		if r.depth >= MaxDepth {
			return Element{}, fmt.Errorf("TLV containers nest deeper than %d levels", MaxDepth)
		}
		r.depth++
		switch elementType {
		case wireStructure:
			element.Type = TypeStructure
		case wireArray:
			element.Type = TypeArray
		default:
			element.Type = TypeList
		}
	case elementType == wireEnd:
		if r.depth == 0 {
			return Element{}, errors.New("TLV end-of-container without an open container")
		}
		r.depth--
		element.Type = TypeEnd
	default:
		return Element{}, fmt.Errorf("reserved TLV element type 0x%02x", elementType)
	}
	return element, nil
}

func (r *Reader) readTag(control byte) (Tag, error) {
	switch control {
	case tagCtrlAnonymous:
		return Anonymous(), nil
	case tagCtrlContext:
		raw, err := r.take(1)
		if err != nil {
			return Tag{}, err
		}
		return Context(raw[0]), nil
	case tagCtrlCommon16, tagCtrlImplicit16:
		raw, err := r.take(2)
		if err != nil {
			return Tag{}, err
		}
		number := uint32(binary.LittleEndian.Uint16(raw))
		if control == tagCtrlCommon16 {
			return Common(number), nil
		}
		return Implicit(number), nil
	case tagCtrlCommon32, tagCtrlImplicit32:
		raw, err := r.take(4)
		if err != nil {
			return Tag{}, err
		}
		number := binary.LittleEndian.Uint32(raw)
		if control == tagCtrlCommon32 {
			return Common(number), nil
		}
		return Implicit(number), nil
	case tagCtrlFull48:
		raw, err := r.take(6)
		if err != nil {
			return Tag{}, err
		}
		return Full(binary.LittleEndian.Uint16(raw), binary.LittleEndian.Uint16(raw[2:]),
			uint32(binary.LittleEndian.Uint16(raw[4:]))), nil
	case tagCtrlFull64:
		raw, err := r.take(8)
		if err != nil {
			return Tag{}, err
		}
		return Full(binary.LittleEndian.Uint16(raw), binary.LittleEndian.Uint16(raw[2:]),
			binary.LittleEndian.Uint32(raw[4:])), nil
	default:
		return Tag{}, fmt.Errorf("reserved TLV tag control 0x%02x", control)
	}
}

func (r *Reader) take(count int) ([]byte, error) {
	if r.pos+count > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	raw := r.data[r.pos : r.pos+count]
	r.pos += count
	return raw, nil
}

func (r *Reader) readLE(widthLog byte) (uint64, error) {
	raw, err := r.take(1 << widthLog)
	if err != nil {
		return 0, err
	}
	switch widthLog {
	case 0:
		return uint64(raw[0]), nil
	case 1:
		return uint64(binary.LittleEndian.Uint16(raw)), nil
	case 2:
		return uint64(binary.LittleEndian.Uint32(raw)), nil
	default:
		return binary.LittleEndian.Uint64(raw), nil
	}
}

func (r *Reader) readLengthPrefixed(widthLog byte) ([]byte, error) {
	length, err := r.readLE(widthLog)
	if err != nil {
		return nil, err
	}
	if length > uint64(len(r.data)-r.pos) {
		return nil, io.ErrUnexpectedEOF
	}
	return r.take(int(length))
}

func signExtend(raw uint64, widthLog byte) int64 {
	switch widthLog {
	case 0:
		return int64(int8(raw))
	case 1:
		return int64(int16(raw))
	case 2:
		return int64(int32(raw))
	default:
		return int64(raw)
	}
}

// Dump decodes a complete TLV sequence into JSON-friendly Go values, mainly
// for CLI inspection and tests. Structures become maps keyed by tag,
// arrays/lists become slices, byte strings become 0x-prefixed hex.
func Dump(data []byte) (any, error) {
	reader := NewReader(data)
	values, err := dumpLevel(reader)
	if err != nil {
		return nil, err
	}
	if len(values) == 1 {
		return values[0].value, nil
	}
	result := make([]any, len(values))
	for index, item := range values {
		result[index] = item.value
	}
	return result, nil
}

type dumped struct {
	tag   Tag
	value any
}

func dumpLevel(reader *Reader) ([]dumped, error) {
	var values []dumped
	for {
		element, err := reader.Next()
		if errors.Is(err, io.EOF) || (err == nil && element.Type == TypeEnd) {
			return values, nil
		}
		if err != nil {
			return nil, err
		}
		value, err := dumpElement(reader, element)
		if err != nil {
			return nil, err
		}
		values = append(values, dumped{tag: element.Tag, value: value})
	}
}

func dumpElement(reader *Reader, element Element) (any, error) {
	switch element.Type {
	case TypeInt:
		return element.Int, nil
	case TypeUint:
		return element.Uint, nil
	case TypeBool:
		return element.Bool, nil
	case TypeFloat:
		return element.Float, nil
	case TypeString:
		return string(element.Data), nil
	case TypeBytes:
		return fmt.Sprintf("0x%x", element.Data), nil
	case TypeNull:
		return nil, nil
	case TypeStructure:
		children, err := dumpLevel(reader)
		if err != nil {
			return nil, err
		}
		object := make(map[string]any, len(children))
		for _, child := range children {
			object[child.tag.String()] = child.value
		}
		return object, nil
	case TypeArray, TypeList:
		children, err := dumpLevel(reader)
		if err != nil {
			return nil, err
		}
		values := make([]any, len(children))
		for index, child := range children {
			values[index] = child.value
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unexpected TLV element type %d", element.Type)
	}
}
