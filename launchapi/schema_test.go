package launchapi

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLaunchAPIV1SchemaContract(t *testing.T) {
	raw, err := os.ReadFile("../schemas/launch-api-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse launch API schema: %v", err)
	}
	defs := objectMap(t, schema["$defs"], "$defs")
	for _, name := range []string{
		"LaunchIntentV1", "ParticipantV1", "WorkingDirectoryV1", "ExecutionOptionsV1",
		"WakeOptionsV1", "InjectorOptionsV1", "IntegrationsV1", "SymphonyOptionsV1",
		"TargetV1", "PrepareRequestV1", "PrepareResultV1", "ApplyRequestV1", "ApplyResultV1",
		"ConfigOverrideCapabilityV1", "ProviderCapabilitiesV1",
		"DecisionV1", "ParticipantObservationV1", "CompatibilityV1", "RequirementV1", "NegotiatedV1",
		"InspectRequestV1", "FocusRequestV1", "CloseRequestV1",
		"InspectResultV1", "FocusResultV1", "CloseResultV1",
	} {
		if defs[name] == nil {
			t.Errorf("schema omits named definition %s", name)
		}
	}
	for name, rawDefinition := range defs {
		definition := objectMap(t, rawDefinition, "$defs."+name)
		if definition["type"] == "object" && definition["additionalProperties"] != false {
			t.Errorf("schema object %s is not fail-closed", name)
		}
	}

	assertSchemaPropertiesMatchType(t, defs, "LaunchIntentV1", reflect.TypeFor[LaunchIntentV1]())
	assertSchemaPropertiesMatchType(t, defs, "PreviewV1", reflect.TypeFor[PreviewV1]())
	assertSchemaPropertiesMatchType(t, defs, "ConfigOverrideCapabilityV1", reflect.TypeFor[ConfigOverrideCapabilityV1]())
	assertSchemaPropertiesMatchType(t, defs, "ProviderCapabilitiesV1", reflect.TypeFor[ProviderCapabilitiesV1]())
	assertSchemaPropertiesMatchType(t, defs, "PrepareResultV1", reflect.TypeFor[PrepareResultV1]())
	assertSchemaPropertiesMatchType(t, defs, "ApplyRequestV1", reflect.TypeFor[ApplyRequestV1]())
	assertSchemaPropertiesMatchType(t, defs, "ApplyResultV1", reflect.TypeFor[ApplyResultV1]())
	runnableFields := jsonFieldNames(reflect.TypeFor[ParticipantV1]())
	assertStringSetsEqual(t, schemaPropertyNames(t, defs, "RunnableParticipantV1"), runnableFields, "runnable participant fields")
	assertStringSetsEqual(
		t,
		schemaPropertyNames(t, defs, "NonRunnableParticipantV1"),
		[]string{"handle", "runnable"},
		"non-runnable participant fields",
	)

	prepareProperties := schemaProperties(t, defs, "PrepareResultV1")
	applyProperties := schemaProperties(t, defs, "ApplyResultV1")
	for _, field := range []string{"subject_digest", "plan_digest", "trust_digest"} {
		want := "#/$defs/Digest"
		if got := objectMap(t, prepareProperties[field], "PrepareResultV1."+field)["$ref"]; got != want {
			t.Errorf("PrepareResultV1.%s ref = %v, want %s", field, got, want)
		}
		if got := objectMap(t, applyProperties[field], "ApplyResultV1."+field)["$ref"]; got != want {
			t.Errorf("ApplyResultV1.%s ref = %v, want %s", field, got, want)
		}
	}
	if got := objectMap(t, applyProperties["semantic_digest"], "ApplyResultV1.semantic_digest")["$ref"]; got != "#/$defs/Digest" {
		t.Errorf("ApplyResultV1.semantic_digest ref = %v", got)
	}
}

func TestPrepareResultMatchesPublishedSchema(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
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
	schema, err := compiler.Compile("launch-api-v1.schema.json#/$defs/PrepareResultV1")
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
		t.Fatalf("schema rejected live Prepare result: %v\n%s", err, rawResult)
	}
}

func TestApplyResultMatchesPublishedSchema(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	schema, err := compiler.Compile("launch-api-v1.schema.json#/$defs/ApplyResultV1")
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
		t.Fatalf("schema rejected live Apply result: %v\n%s", err, rawResult)
	}
}

func TestLaunchAPIV1SchemaAcceptsGoldenAndRejectsHostileFields(t *testing.T) {
	raw, err := os.ReadFile("../schemas/launch-api-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(raw, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("launch-api-v1.schema.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("launch-api-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	var golden map[string]any
	if err := json.Unmarshal([]byte(validIntentJSON(t)), &golden); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(golden); err != nil {
		t.Fatalf("schema rejected package golden: %v", err)
	}

	participants := golden["participants"].([]any)
	runnable := participants[1].(map[string]any)
	for _, field := range []string{"adapter_mode", "launch_nonce", "conversation_id", "dynamic_argv"} {
		runnable[field] = "forged"
		if err := schema.Validate(golden); err == nil {
			t.Errorf("schema accepted AMQ-owned field %q", field)
		}
		delete(runnable, field)
	}
	nonRunnable := participants[0].(map[string]any)
	nonRunnable["args"] = []any{}
	if err := schema.Validate(golden); err == nil {
		t.Error("schema accepted non-runnable args smuggling")
	}

	for _, resultGolden := range []struct {
		file       string
		definition string
	}{
		{file: "prepare_result_v1.golden.json", definition: "PrepareResultV1"},
		{file: "apply_result_v1.golden.json", definition: "ApplyResultV1"},
	} {
		data, err := os.ReadFile("testdata/" + resultGolden.file)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		resultSchema, err := compiler.Compile("launch-api-v1.schema.json#/$defs/" + resultGolden.definition)
		if err != nil {
			t.Fatal(err)
		}
		if err := resultSchema.Validate(document); err != nil {
			t.Fatalf("schema rejected %s: %v", resultGolden.file, err)
		}
		document["caller_owned_backend_state"] = true
		if err := resultSchema.Validate(document); err == nil {
			t.Errorf("schema accepted hostile field in %s", resultGolden.file)
		}
	}
}

func assertSchemaPropertiesMatchType(t *testing.T, defs map[string]any, definition string, typ reflect.Type) {
	t.Helper()
	assertStringSetsEqual(t, schemaPropertyNames(t, defs, definition), jsonFieldNames(typ), definition+" fields")
}

func schemaPropertyNames(t *testing.T, defs map[string]any, definition string) []string {
	t.Helper()
	properties := schemaProperties(t, defs, definition)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func schemaProperties(t *testing.T, defs map[string]any, definition string) map[string]any {
	t.Helper()
	def := objectMap(t, defs[definition], "$defs."+definition)
	return objectMap(t, def["properties"], "$defs."+definition+".properties")
}

func jsonFieldNames(typ reflect.Type) []string {
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func assertStringSetsEqual(t *testing.T, got, want []string, context string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", context, got, want)
	}
}

func objectMap(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", context, value)
	}
	return object
}
