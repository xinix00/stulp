package webapi

import (
	"encoding/json"
	"testing"
)

// The catalog feeds two consumers: tools/list serves the encoded form, and
// argument validation uses the same schemas. This checks they cannot drift.
func TestMCPToolCatalogFeedsListingAndValidation(t *testing.T) {
	t.Parallel()
	var listed []map[string]any
	if err := json.Unmarshal(mcpToolCatalogJSON, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(mcpToolCatalog) || len(mcpToolInputSchemas) != len(mcpToolCatalog) {
		t.Fatalf("catalog has %d tools, %d encoded, %d validated",
			len(mcpToolCatalog), len(listed), len(mcpToolInputSchemas))
	}
	if len(mcpToolHandlers) != len(mcpToolCatalog) {
		t.Fatalf("%d tools advertised, %d dispatched", len(mcpToolCatalog), len(mcpToolHandlers))
	}
	for _, tool := range listed {
		name, _ := tool["name"].(string)
		schema, ok := mcpToolInputSchemas[name]
		if !ok {
			t.Fatalf("advertised tool %q has no validation schema", name)
		}
		if mcpToolHandlers[name] == nil {
			t.Fatalf("advertised tool %q has no handler", name)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q has a non-object root schema", name)
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		advertised, err := json.Marshal(tool["inputSchema"])
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != string(advertised) {
			t.Errorf("tool %q validates against a different schema than it advertises:\n%s\n%s", name, encoded, advertised)
		}
	}
}
