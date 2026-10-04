package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Separate candidate fixtures cannot silently enter the published 3.5 validator.
func TestWritePrivateAutoWireFixtures(t *testing.T) {
	var source PrivateAutoSourceApproval
	if err := json.Unmarshal(privateAutoGolden(t, "approval"), &source); err != nil {
		t.Fatal(err)
	}
	var first, unicode PrivateAutoRequest
	if err := json.Unmarshal(privateAutoGolden(t, "first"), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(privateAutoGolden(t, "unicode"), &unicode); err != nil {
		t.Fatal(err)
	}
	valid := []fixture{
		{Schema: "privateAutoSourceApprovalSchema", Case: "source", Value: source},
		{Schema: "privateAutoExecutionSchema", Case: "child", Value: first.PrivateAutoExecution},
		{Schema: "privateAutoPrincipalSchema", Case: "principal", Value: source.Principal},
		{Schema: "privateAutoInputSchema", Case: "input", Value: first.Input},
		{Schema: "privateAutoInferenceRequestSchema", Case: "go-envelope", Value: first},
		{Schema: "privateAutoInferenceRequestSchema", Case: "go-unicode-envelope", Value: unicode},
	}
	mutate := func(value any, change func(map[string]any)) any {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		change(out)
		return out
	}
	invalid := []fixture{
		{Schema: "privateAutoSourceApprovalSchema", Case: "retention", Value: mutate(source, func(v map[string]any) { v["review"].(map[string]any)["retainsPayloads"] = true })},
		{Schema: "privateAutoSourceApprovalSchema", Case: "missing-review", Value: mutate(source, func(v map[string]any) { delete(v, "review") })},
		{Schema: "privateAutoSourceApprovalSchema", Case: "caller-input-authority", Value: mutate(source, func(v map[string]any) { v["inputSha256"] = "caller" })},
		{Schema: "privateAutoExecutionSchema", Case: "new-operation", Value: mutate(first.PrivateAutoExecution, func(v map[string]any) { v["operationId"] = "replacement" })},
		{Schema: "privateAutoExecutionSchema", Case: "after-source", Value: mutate(first.PrivateAutoExecution, func(v map[string]any) { v["runtimeExpiresAt"] = "2031-01-01T00:00:00Z" })},
		{Schema: "privateAutoInferenceRequestSchema", Case: "delegation", Value: mutate(first, func(v map[string]any) { v["attribution"].(map[string]any)["userId"] = "synthetic-foreign-user" })},
		{Schema: "privateAutoInferenceRequestSchema", Case: "foreign-principal", Value: mutate(first, func(v map[string]any) {
			v["attribution"].(map[string]any)["principal"].(map[string]any)["applicationId"] = "synthetic-foreign-app"
		})},
		{Schema: "privateAutoInferenceRequestSchema", Case: "wrong-version", Value: mutate(first, func(v map[string]any) { v["schemaVersion"] = 3 })},
	}
	root := fixtureOutputRoot(t)
	writeFixtures(t, root+"/valid", valid)
	writeFixtures(t, root+"/invalid", invalid)
}

// The canonical generated candidate descriptor is separate from descriptor.json.
// Check its four newly exchanged named shapes with the existing structural comparator.
func TestPrivateAutoCandidateDescriptor(t *testing.T) {
	path := os.Getenv("KAANA_PRIVATE_AUTO_CANDIDATE_DESCRIPTOR")
	if path == "" {
		t.Skip("candidate descriptor is supplied by the explicit provisional validator")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file descriptorFile
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Source struct {
			Kind   string `json:"kind"`
			Origin string `json:"origin"`
		} `json:"source"`
	}
	if err = json.Unmarshal(raw, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Source.Kind != "unpublished-local-build" || provenance.Source.Origin != os.Getenv("OXY_CONTRACTS_LOCAL_SOURCE") {
		t.Fatal("candidate provenance differs")
	}
	shapes := map[string]reflect.Type{
		"privateAutoSourceApprovalSchema": reflect.TypeFor[PrivateAutoSourceApproval](),
		"privateAutoExecutionSchema":      reflect.TypeFor[PrivateAutoExecution](),
		"privateAutoPrincipalSchema":      reflect.TypeFor[PrivateAutoPrincipal](),
		"privateAutoInputSchema":          reflect.TypeFor[Input](),
	}
	// Nested exported references use the same registered production Go types.
	for name, typ := range shapes {
		if _, exists := goShapes[name]; exists {
			t.Fatal("candidate unexpectedly registered as published")
		}
		goShapes[name] = typ
	}
	defer func() {
		for name := range shapes {
			delete(goShapes, name)
		}
	}()
	checker := shapeChecker{descriptor: file}
	for name, typ := range shapes {
		node, ok := file.Shapes[name]
		if !ok {
			t.Fatalf("candidate omits %s", name)
		}
		// private input deliberately excludes text/messages/batch formats through
		// strict decoding; actual values are tested against Zod below. The legacy
		// shared Input struct carries those other variants, so only its two private
		// fields are structurally compared here.
		if name == "privateAutoInputSchema" {
			typ = reflect.TypeOf(struct {
				Format    InputFormat   `json:"format"`
				Decisions DecisionInput `json:"decisions"`
			}{})
		}
		for _, problem := range checker.compareShape(name, node, typ) {
			t.Error(problem)
		}
	}
}
