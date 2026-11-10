package launchapi

import (
	"encoding/json"
	"os"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPublishedSchemaCompiles(t *testing.T) {
	t.Helper()
	rawSchema, err := os.ReadFile("../schemas/launch-api-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(rawSchema, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("launch-api-v1.schema.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []string{"PrepareResultV1", "ApplyResultV1"} {
		if _, err := compiler.Compile("launch-api-v1.schema.json#/$defs/" + definition); err != nil {
			t.Fatalf("compile %s: %v", definition, err)
		}
	}
}
