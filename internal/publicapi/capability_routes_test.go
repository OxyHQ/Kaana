package publicapi

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// capabilityLine serves one line from three deployments in inventory order:
// the first states it takes no temperature, the second says nothing, the third
// takes it.
func capabilityLine(t *testing.T, first, third string) *inventory.Inventory {
	t.Helper()
	deployment := func(id, slug string, accepted *string) map[string]any {
		entry := map[string]any{
			"deploymentId": id, "provider": slug, "current": true,
			"modelReference": "acme/chat@observed-2026-11-12", "upstreamModelId": "chat",
		}
		if accepted != nil {
			entry["observed"] = json.RawMessage(`{"acceptedParameters":` + *accepted + `}`)
		}
		return entry
	}
	document, err := json.Marshal(map[string]any{
		"snapshotId": "snap_capability", "issuedAt": contract.NewTimestamp(time.Now()),
		"deployments": []map[string]any{
			deployment("dep_refuses", "alpha", &first),
			deployment("dep_silent", "beta", nil),
			deployment("dep_accepts", "gamma", &third),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := inventory.Parse(document, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func capabilityGrant() *contract.Grant {
	line := contract.ModelID("acme/chat")
	return &contract.Grant{
		RequestID: "req_capability", Operation: contract.GrantChatCompletions,
		Steps: []contract.GrantStep{{
			Target: contract.GrantTarget{Kind: contract.GrantTargetModel, ModelLine: &line},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: line, PriceVersionID: "price"}}},
		}},
	}
}

func routedFor(t *testing.T, line *inventory.Inventory, needs []provider.RequestParameter) []contract.DeploymentID {
	t.Helper()
	resolved, failure := resolveGrant(capabilityGrant(), line, nil, nil, needs, time.Now())
	if failure != nil {
		t.Fatalf("resolveGrant refused: %v", failure)
	}
	var deployments []contract.DeploymentID
	for _, route := range resolved.routes {
		deployments = append(deployments, route.DeploymentID)
	}
	return deployments
}

// TestAGrantSkipsADeploymentThatRefusesAControlTheRequestCarries: a deployment
// whose published set lacks the control is not attempted while another admitted
// route can take it (ADR 0008); one that says nothing stays.
func TestAGrantSkipsADeploymentThatRefusesAControlTheRequestCarries(t *testing.T) {
	line := capabilityLine(t, `["tools"]`, `["sampling.temperature","tools"]`)
	temperature := []provider.RequestParameter{provider.ParameterTemperature}

	if got := routedFor(t, line, temperature); !slices.Equal(got, []contract.DeploymentID{"dep_silent", "dep_accepts"}) {
		t.Errorf("a request with a temperature was routed to %v", got)
	}
	// Positive control: with no control carried, nothing is skipped.
	if got := routedFor(t, line, nil); !slices.Equal(got, []contract.DeploymentID{"dep_refuses", "dep_silent", "dep_accepts"}) {
		t.Errorf("a request carrying nothing was routed to %v", got)
	}
}

// TestAGrantFallsBackWhenNoDeploymentAcceptsTheControl: the preference never
// turns a request into "no route". When the grant leaves only deployments that
// refuse the control, they are kept and the attempt refuses with the exact
// parameter named, as before.
func TestAGrantFallsBackWhenNoDeploymentAcceptsTheControl(t *testing.T) {
	line := capabilityLine(t, `["tools"]`, `["tools"]`)
	grant := capabilityGrant()
	grant.Steps[0].Limits.ProviderAllowlist = []contract.ProviderSlug{"alpha", "gamma"}

	resolved, failure := resolveGrant(grant, line, nil, nil, []provider.RequestParameter{provider.ParameterTemperature}, time.Now())
	if failure != nil {
		t.Fatalf("a request every admitted deployment refuses became %v instead of reaching the attempt", failure)
	}
	if len(resolved.routes) != 2 || resolved.routes[0].DeploymentID != "dep_refuses" {
		t.Errorf("routes = %v", resolved.routes)
	}
}

func TestCarriedParametersReadsTheWireTheWayTheAdapterDoes(t *testing.T) {
	none, required := contract.ToolChoiceNone, contract.ToolChoiceRequired
	auto := contract.ToolChoiceAuto
	max := 10
	temperature := 0.2
	text := &contract.ResponseFormat{Type: contract.ResponseFormatText}
	object := &contract.ResponseFormat{Type: contract.ResponseFormatJSONObject}

	for name, c := range map[string]struct {
		got  []provider.RequestParameter
		want []provider.RequestParameter
	}{
		"nothing":            {carriedParameters(nil, contract.SamplingParameters{}, 0, nil, nil, false), nil},
		"text sends nothing": {carriedParameters(nil, contract.SamplingParameters{}, 0, nil, text, false), nil},
		"auto sends nothing": {carriedParameters(nil, contract.SamplingParameters{}, 1, &contract.ToolChoice{Mode: &auto}, nil, false), []provider.RequestParameter{provider.ParameterTools}},
		"a json object":      {carriedParameters(nil, contract.SamplingParameters{}, 0, nil, object, false), []provider.RequestParameter{provider.ParameterResponseFormat}},
		"limits and sampling": {carriedParameters(&max, contract.SamplingParameters{Temperature: &temperature}, 0, nil, nil, true),
			[]provider.RequestParameter{provider.ParameterMaxOutputTokens, provider.ParameterReasoningEffort, provider.ParameterTemperature}},
		"none":     {carriedParameters(nil, contract.SamplingParameters{}, 1, &contract.ToolChoice{Mode: &none}, nil, false), []provider.RequestParameter{provider.ParameterTools, provider.ParameterToolChoiceNone}},
		"required": {carriedParameters(nil, contract.SamplingParameters{}, 1, &contract.ToolChoice{Mode: &required}, nil, false), []provider.RequestParameter{provider.ParameterTools, provider.ParameterToolChoiceRequired}},
		"function": {carriedParameters(nil, contract.SamplingParameters{}, 1, &contract.ToolChoice{Function: &contract.ToolChoiceFunction{Name: "f"}}, nil, false), []provider.RequestParameter{provider.ParameterTools, provider.ParameterToolChoiceFunction}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
}
