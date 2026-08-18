package im

import (
	"errors"
	"fmt"
	"io"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// Value is a lossless-enough tree view of Matter TLV for the Interaction
// Model. It preserves types, tags and repeated children, unlike the
// JSON-oriented tlv.Dump helper.
type Value struct {
	Tag      tlv.Tag
	Type     tlv.Type
	Int      int64
	Uint     uint64
	Bool     bool
	Float    float64
	Data     []byte
	Children []Value
}

func decodeTree(data []byte) (Value, error) {
	reader := tlv.NewReader(data)
	root, err := readValue(reader)
	if err != nil {
		return Value{}, err
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return Value{}, errors.New("Interaction Model message has trailing TLV elements")
		}
		return Value{}, err
	}
	return root, nil
}

func readValue(reader *tlv.Reader) (Value, error) {
	element, err := reader.Next()
	if err != nil {
		return Value{}, err
	}
	if element.Type == tlv.TypeEnd {
		return Value{}, errors.New("unexpected end-of-container")
	}
	value := Value{
		Tag: element.Tag, Type: element.Type, Int: element.Int, Uint: element.Uint,
		Bool: element.Bool, Float: element.Float, Data: element.Data,
	}
	if !isContainer(element.Type) {
		return value, nil
	}
	for {
		next, err := reader.Next()
		if err != nil {
			return Value{}, err
		}
		if next.Type == tlv.TypeEnd {
			return value, nil
		}
		child, err := readElement(reader, next)
		if err != nil {
			return Value{}, err
		}
		value.Children = append(value.Children, child)
	}
}

func readElement(reader *tlv.Reader, element tlv.Element) (Value, error) {
	value := Value{
		Tag: element.Tag, Type: element.Type, Int: element.Int, Uint: element.Uint,
		Bool: element.Bool, Float: element.Float, Data: element.Data,
	}
	if !isContainer(element.Type) {
		return value, nil
	}
	for {
		next, err := reader.Next()
		if err != nil {
			return Value{}, err
		}
		if next.Type == tlv.TypeEnd {
			return value, nil
		}
		child, err := readElement(reader, next)
		if err != nil {
			return Value{}, err
		}
		value.Children = append(value.Children, child)
	}
}

func isContainer(kind tlv.Type) bool {
	return kind == tlv.TypeStructure || kind == tlv.TypeArray || kind == tlv.TypeList
}

// Field returns the first context-tagged child with number. Cluster-specific
// clients use this to consume command fields without teaching the generic
// Interaction Model package every Matter cluster schema.
func (v Value) Field(number uint8) (Value, bool) {
	for _, child := range v.Children {
		if tag, ok := child.Tag.ContextNumber(); ok && tag == number {
			return child, true
		}
	}
	return Value{}, false
}

func requiredField(value Value, number uint8, kind tlv.Type) (Value, error) {
	field, ok := value.Field(number)
	if !ok {
		return Value{}, fmt.Errorf("missing field %d", number)
	}
	if field.Type != kind {
		return Value{}, fmt.Errorf("field %d has TLV type %d, want %d", number, field.Type, kind)
	}
	return field, nil
}

func uintField(value Value, number uint8, required bool) (uint64, bool, error) {
	field, ok := value.Field(number)
	if !ok {
		if required {
			return 0, false, fmt.Errorf("missing unsigned field %d", number)
		}
		return 0, false, nil
	}
	if field.Type != tlv.TypeUint {
		return 0, false, fmt.Errorf("field %d is not unsigned", number)
	}
	return field.Uint, true, nil
}
