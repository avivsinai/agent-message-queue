package launchapi

import (
	"encoding/json"
	"os"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func assertLiveLifecycleResultMatchesPublishedSchema(t *testing.T, definition string, result any) {
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
	schema, err := compiler.Compile("launch-api-v1.schema.json#/$defs/" + definition)
	if err != nil {
		t.Fatal(err)
	}
	rawResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(rawResult, &document); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("live %s rejected by published schema: %v\n%s", definition, err, rawResult)
	}
}
