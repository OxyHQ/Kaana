package contract

import (
	"encoding/json"
	"testing"
)

// Private contract fixtures are validated against the exact installed published package.
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
	rawInput, err := json.Marshal(first.Input)
	if err != nil {
		t.Fatal(err)
	}
	privateInput, err := decodePrivateAutoInput(rawInput)
	if err != nil {
		t.Fatal(err)
	}
	valid := []fixture{
		{Schema: "privateAutoSourceApprovalSchema", Case: "source", Value: source},
		{Schema: "privateAutoExecutionSchema", Case: "child", Value: first.PrivateAutoExecution},
		{Schema: "privateAutoPrincipalSchema", Case: "principal", Value: source.Principal},
		{Schema: "privateAutoInputSchema", Case: "input", Value: privateInput},
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
