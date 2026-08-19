package webapi

import (
	"encoding/json"
	"fmt"
	"math"
	"unicode/utf8"
)

const (
	mcpDefaultPageLimit    = 50
	mcpMaximumPageLimit    = 100
	mcpMaximumStringBytes  = 16 << 10
	mcpMaximumObjectKey    = 256
	mcpMaximumContainer    = 512
	mcpMaximumValueDepth   = 12
	mcpMaximumValueCount   = 8192
	mcpMaximumValueBytes   = 64 << 10
	mcpMaximumEnumValues   = 128
	mcpMaximumDisplayRunes = 500
)

// mcpValueBudget bounds a whole argument tree instead of only its individual
// values: a thousand strings just under the string limit is as expensive as one
// oversized string, and only a shared budget notices that.
type mcpValueBudget struct {
	values int
	bytes  int
}

func newMCPValueBudget() mcpValueBudget {
	return mcpValueBudget{values: mcpMaximumValueCount, bytes: mcpMaximumValueBytes}
}

func (budget *mcpValueBudget) takeValue() bool {
	budget.values--
	budget.bytes -= 4 // JSON punctuation or a small scalar.
	return budget.values >= 0 && budget.bytes >= 0
}

func (budget *mcpValueBudget) takeBytes(count int) bool {
	budget.bytes -= count
	return budget.bytes >= 0
}

func validateMCPToolArguments(name string, arguments map[string]any) error {
	schema, known := mcpToolInputSchemas[name]
	if !known {
		return fmt.Errorf("unknown MCP tool %q", name)
	}
	budget := newMCPValueBudget()
	return validateMCPValue(arguments, schema, "arguments", 0, &budget)
}

// validateMCPValue checks one value against the JSON-Schema vocabulary the
// catalog actually uses, and bounds it whether or not a schema declares it: a
// card's args come from an app manifest, so no schema can name their fields,
// but they still have to fit in depth, size and count.
func validateMCPValue(value any, schema map[string]any, path string, depth int, budget *mcpValueBudget) error {
	if depth > mcpMaximumValueDepth {
		return fmt.Errorf("%s exceeds the maximum nesting depth", path)
	}
	if !budget.takeValue() {
		return fmt.Errorf("%s exceeds the total value budget", path)
	}
	declared, _ := schema["type"].(string)

	switch typed := value.(type) {
	case nil:
		if declared != "" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
		return nil
	case bool:
		if declared != "" && declared != "boolean" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
	case string:
		if declared != "" && declared != "string" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
		if err := validateMCPString(typed, schema, path, budget); err != nil {
			return err
		}
	case map[string]any:
		if declared != "" && declared != "object" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
		if err := validateMCPObject(typed, schema, path, depth, budget); err != nil {
			return err
		}
	case []any:
		if declared != "" && declared != "array" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
		if err := validateMCPArray(typed, schema, path, depth, budget); err != nil {
			return err
		}
	case int, int64, float32, float64, json.Number:
		if declared != "" && declared != "number" && declared != "integer" {
			return fmt.Errorf("%s must be a %s", path, declared)
		}
		if err := validateMCPNumber(typed, declared, schema, path, budget); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s contains unsupported value type %T", path, value)
	}

	if enum := schema["enum"]; enum != nil && !mcpEnumContains(enum, value) {
		return fmt.Errorf("%s is not an advertised enum value", path)
	}
	return nil
}

func validateMCPString(text string, schema map[string]any, path string, budget *mcpValueBudget) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%s is not valid UTF-8", path)
	}
	if len(text) > mcpMaximumStringBytes {
		return fmt.Errorf("%s is larger than %d bytes", path, mcpMaximumStringBytes)
	}
	if !budget.takeBytes(len(text)) {
		return fmt.Errorf("%s exceeds the total value budget", path)
	}
	length := utf8.RuneCountInString(text)
	if minimum, ok := mcpSchemaInteger(schema["minLength"]); ok && length < minimum {
		return fmt.Errorf("%s needs at least %d characters", path, minimum)
	}
	if maximum, ok := mcpSchemaInteger(schema["maxLength"]); ok && length > maximum {
		return fmt.Errorf("%s allows at most %d characters", path, maximum)
	}
	return nil
}

func validateMCPObject(object map[string]any, schema map[string]any, path string, depth int, budget *mcpValueBudget) error {
	if len(object) > mcpMaximumContainer {
		return fmt.Errorf("%s has more than %d fields", path, mcpMaximumContainer)
	}
	for _, field := range mcpSchemaStrings(schema["required"]) {
		if _, exists := object[field]; !exists {
			return fmt.Errorf("%s.%s is required", path, field)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	// A schema without properties describes free-form values, so unknown fields
	// are the whole point there; only a schema that names its fields can reject.
	allowUnknown := properties == nil
	if allow, ok := schema["additionalProperties"].(bool); ok {
		allowUnknown = allow
	}
	for field, child := range object {
		if !utf8.ValidString(field) || len(field) > mcpMaximumObjectKey {
			return fmt.Errorf("%s contains an invalid or oversized field name", path)
		}
		if !budget.takeBytes(len(field)) {
			return fmt.Errorf("%s exceeds the total value budget", path)
		}
		childSchema, named := properties[field].(map[string]any)
		if !named && !allowUnknown {
			return fmt.Errorf("unknown field %q in %s", field, path)
		}
		if err := validateMCPValue(child, childSchema, path+"."+field, depth+1, budget); err != nil {
			return err
		}
	}
	return nil
}

func validateMCPArray(values []any, schema map[string]any, path string, depth int, budget *mcpValueBudget) error {
	if len(values) > mcpMaximumContainer {
		return fmt.Errorf("%s has more than %d values", path, mcpMaximumContainer)
	}
	if minimum, ok := mcpSchemaInteger(schema["minItems"]); ok && len(values) < minimum {
		return fmt.Errorf("%s needs at least %d values", path, minimum)
	}
	if maximum, ok := mcpSchemaInteger(schema["maxItems"]); ok && len(values) > maximum {
		return fmt.Errorf("%s allows at most %d values", path, maximum)
	}
	itemSchema, _ := schema["items"].(map[string]any)
	for index, child := range values {
		if err := validateMCPValue(child, itemSchema, fmt.Sprintf("%s[%d]", path, index), depth+1, budget); err != nil {
			return err
		}
	}
	return nil
}

func validateMCPNumber(value any, declared string, schema map[string]any, path string, budget *mcpValueBudget) error {
	number, ok := mcpFiniteNumber(value)
	if !ok {
		return fmt.Errorf("%s must be a finite number", path)
	}
	if declared == "integer" && math.Trunc(number) != number {
		return fmt.Errorf("%s must be a whole number", path)
	}
	if minimum, ok := mcpFiniteNumber(schema["minimum"]); ok && number < minimum {
		return fmt.Errorf("%s must be at least %v", path, schema["minimum"])
	}
	if maximum, ok := mcpFiniteNumber(schema["maximum"]); ok && number > maximum {
		return fmt.Errorf("%s must be at most %v", path, schema["maximum"])
	}
	if !budget.takeBytes(mcpNumberBytes(value)) {
		return fmt.Errorf("%s exceeds the total value budget", path)
	}
	return nil
}

func mcpSchemaStrings(value any) []string {
	if values, ok := value.([]string); ok {
		return values
	}
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func mcpSchemaInteger(value any) (int, bool) {
	number, ok := mcpFiniteNumber(value)
	if !ok || math.Trunc(number) != number || number < 0 || number > math.MaxInt32 {
		return 0, false
	}
	return int(number), true
}

// mcpPage reads the paging arguments. Their type and range were already checked
// against the catalog, so anything unusable here simply falls back.
func mcpPage(arguments map[string]any) (offset, limit int) {
	offset, _ = mcpSchemaInteger(arguments["offset"])
	limit, ok := mcpSchemaInteger(arguments["limit"])
	if !ok || limit < 1 {
		limit = mcpDefaultPageLimit
	}
	if limit > mcpMaximumPageLimit {
		limit = mcpMaximumPageLimit
	}
	return offset, limit
}

// mcpPageValues returns one page and the offset of the next one, or -1 when the
// page is the last.
func mcpPageValues[T any](values []T, offset, limit int) ([]T, int) {
	if offset >= len(values) {
		return nil, -1
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	next := -1
	if end < len(values) {
		next = end
	}
	return values[offset:end], next
}

// mcpSafeValue passes a value through for public MCP output, or returns nil when
// it is not a bounded JSON value. Callers own their value already -- flows are
// cloned by the store, run results are freshly built -- so this bounds rather
// than copies.
func mcpSafeValue(value any, depth int) any {
	return mcpSafeValueLimit(value, depth, mcpMaximumValueCount, mcpMaximumValueBytes)
}

func mcpSafeValueLimit(value any, depth, maximumValues, maximumBytes int) any {
	budget := mcpValueBudget{values: maximumValues, bytes: maximumBytes}
	if err := validateMCPValue(value, nil, "value", depth, &budget); err != nil {
		return nil
	}
	return value
}

func mcpEnumValues(value any, language string) []any {
	var result []any
	appendValue := func(raw any) {
		id, titleSource := raw, any(nil)
		if object, ok := raw.(map[string]any); ok {
			var exists bool
			id, exists = object["id"]
			if !exists {
				return
			}
			titleSource = object["title"]
		}
		safeID, ok := mcpSafeEnumScalar(id)
		if !ok {
			return
		}
		title := ""
		if safeTitle := mcpSafeValue(titleSource, 0); safeTitle != nil {
			title = localized(safeTitle, language)
		}
		if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > mcpMaximumDisplayRunes {
			title = fmt.Sprint(safeID)
		}
		result = append(result, map[string]any{"id": safeID, "title": title})
	}
	switch values := value.(type) {
	case []any:
		for index, raw := range values {
			if index >= mcpMaximumEnumValues {
				break
			}
			appendValue(raw)
		}
	case []string:
		for index, raw := range values {
			if index >= mcpMaximumEnumValues {
				break
			}
			appendValue(raw)
		}
	}
	return result
}

func mcpSafeEnumScalar(value any) (any, bool) {
	if text, ok := value.(string); ok {
		return text, text != "" && utf8.ValidString(text) && utf8.RuneCountInString(text) <= mcpMaximumObjectKey
	}
	if _, ok := value.(bool); ok {
		return value, true
	}
	if _, ok := mcpFiniteNumber(value); ok {
		return value, true
	}
	return nil, false
}

func validateMCPCapabilityValue(capability map[string]any, value any) error {
	typeName, _ := capability["type"].(string)
	switch typeName {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("must be a boolean")
		}
	case "number":
		number, ok := mcpFiniteNumber(value)
		if !ok {
			return fmt.Errorf("must be a finite number")
		}
		if minimum, ok := mcpFiniteNumber(capability["min"]); ok && number < minimum {
			return fmt.Errorf("must be at least %v", capability["min"])
		}
		if maximum, ok := mcpFiniteNumber(capability["max"]); ok && number > maximum {
			return fmt.Errorf("must be at most %v", capability["max"])
		}
	case "string":
		text, ok := value.(string)
		if !ok || !utf8.ValidString(text) {
			return fmt.Errorf("must be a string")
		}
		if len(text) > mcpMaximumStringBytes {
			return fmt.Errorf("must contain at most %d bytes", mcpMaximumStringBytes)
		}
	case "enum":
		if _, ok := mcpSafeEnumScalar(value); !ok {
			return fmt.Errorf("must be a string, boolean or finite number enum id")
		}
		if !mcpEnumContains(capability["values"], value) {
			return fmt.Errorf("must be one of the advertised enum values")
		}
	default:
		return fmt.Errorf("unsupported capability type %q", typeName)
	}
	return nil
}

func mcpEnumContains(rawValues, wanted any) bool {
	check := func(raw any) bool {
		if object, ok := raw.(map[string]any); ok {
			raw = object["id"]
		}
		return mcpScalarEqual(raw, wanted)
	}
	switch values := rawValues.(type) {
	case []any:
		for index, raw := range values {
			if index >= mcpMaximumEnumValues {
				break
			}
			if check(raw) {
				return true
			}
		}
	case []string:
		for index, raw := range values {
			if index >= mcpMaximumEnumValues {
				break
			}
			if check(raw) {
				return true
			}
		}
	}
	return false
}

func mcpScalarEqual(left, right any) bool {
	if leftNumber, ok := mcpFiniteNumber(left); ok {
		rightNumber, rightOK := mcpFiniteNumber(right)
		return rightOK && leftNumber == rightNumber
	}
	switch left := left.(type) {
	case string:
		right, ok := right.(string)
		return ok && left == right
	case bool:
		right, ok := right.(bool)
		return ok && left == right
	}
	return false
}

func mcpFiniteNumber(value any) (float64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		if len(typed) > 64 {
			return 0, false
		}
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

// mcpNumberBytes conservatively covers the JSON representation of every
// machine-sized integer and floating-point value.
func mcpNumberBytes(value any) int {
	if number, ok := value.(json.Number); ok {
		return len(number)
	}
	return 32
}
