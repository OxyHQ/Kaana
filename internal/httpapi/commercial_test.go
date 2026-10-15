package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/OxyHQ/Kaana/internal/commercial"
)

const commercialFile = `{"$comment":"test","schemaVersion":1,"deployments":[
 {"deploymentId":"dep_sold","commercial":{"availabilityScope":"enterprise","commercialPermission":"wholesale_contract","dedicatedCapacity":true}},
 {"deploymentId":"dep_sold_plain","commercial":{"availabilityScope":"platform_internal","commercialPermission":"standard_application_use"}}
]}`

func commercialHarness(t *testing.T) *harness {
	t.Helper()
	table, err := commercial.Parse([]byte(commercialFile))
	if err != nil {
		t.Fatal(err)
	}
	deployment := func(id, line string) map[string]any {
		return map[string]any{"deploymentId": id, "provider": "stub", "modelReference": "stub/" + line + "@2026-05-01", "upstreamModelId": "up-" + line, "regions": []string{"r"}, "current": true}
	}
	return newHarnessWithCommercial(t, &stubAdapter{}, []map[string]any{
		deployment("dep_sold", "a"), deployment("dep_sold_plain", "b"), deployment("dep_unsold", "c"),
	}, table)
}

func queryDescriptors(t *testing.T, h *harness, path, body string) map[string]map[string]json.RawMessage {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	h.sign(request, []byte(body))
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", path, response.StatusCode, raw)
	}
	var parsed struct {
		Deployments []map[string]json.RawMessage `json:"deployments"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, descriptor := range parsed.Deployments {
		var id string
		_ = json.Unmarshal(descriptor["deploymentId"], &id)
		byID[id] = descriptor
	}
	return byID
}

// `commercial` is on the descriptor of every listed deployment and absent on
// every other, on both surfaces Oxy reads: the exact descriptors and the
// negotiated models query.
func TestDescriptorsCarryCommercialOnlyForListedDeployments(t *testing.T) {
	h := commercialHarness(t)
	for name, call := range map[string]struct{ path, body string }{
		"deployments": {"/internal/v1/deployments/query", `{}`},
		"models":      {"/internal/v1/models/query", `{"scopedExecutionContractVersion":"3.6.0"}`},
	} {
		t.Run(name, func(t *testing.T) {
			descriptors := queryDescriptors(t, h, call.path, call.body)
			if len(descriptors) != 3 {
				t.Fatalf("descriptors = %d", len(descriptors))
			}
			if got := string(descriptors["dep_sold"]["commercial"]); got != `{"availabilityScope":"enterprise","commercialPermission":"wholesale_contract","dedicatedCapacity":true}` {
				t.Errorf("dep_sold commercial = %s", got)
			}
			if got := string(descriptors["dep_sold_plain"]["commercial"]); got != `{"availabilityScope":"platform_internal","commercialPermission":"standard_application_use"}` {
				t.Errorf("dep_sold_plain commercial = %s", got)
			}
			if _, present := descriptors["dep_unsold"]["commercial"]; present {
				t.Errorf("an unlisted deployment carries commercial: %s", descriptors["dep_unsold"]["commercial"])
			}
		})
	}
	exact := queryDescriptors(t, h, "/internal/v1/deployments/query", `{"deploymentId":"dep_sold"}`)
	if len(exact) != 1 || exact["dep_sold"]["commercial"] == nil {
		t.Errorf("the exact lookup lost commercial: %v", exact)
	}
}

func TestWithoutTheFileNoDescriptorCarriesCommercial(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	for id, descriptor := range queryDescriptors(t, h, "/internal/v1/deployments/query", `{}`) {
		if _, present := descriptor["commercial"]; present {
			t.Errorf("%s carries commercial with no file wired", id)
		}
	}
}
