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

func routedFor(t *testing.T, line *inventory.Inventory, needs requestNeeds) []contract.DeploymentID {
	t.Helper()
	resolved, failure := resolveGrant(capabilityGrant(), line, nil, nil, nil, needs, time.Now())
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

	if got := routedFor(t, line, requestNeeds{parameters: temperature}); !slices.Equal(got, []contract.DeploymentID{"dep_silent", "dep_accepts"}) {
		t.Errorf("a request with a temperature was routed to %v", got)
	}
	// Positive control: with no control carried, nothing is skipped.
	if got := routedFor(t, line, requestNeeds{}); !slices.Equal(got, []contract.DeploymentID{"dep_refuses", "dep_silent", "dep_accepts"}) {
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

	resolved, failure := resolveGrant(grant, line, nil, nil, nil, requestNeeds{parameters: []provider.RequestParameter{provider.ParameterTemperature}}, time.Now())
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

type fixedLatency map[contract.DeploymentID]time.Duration

func (f fixedLatency) FirstOutput(id contract.DeploymentID) (time.Duration, bool) {
	d, ok := f[id]
	return d, ok
}

type tierByDeployment map[contract.DeploymentID]int

func (f tierByDeployment) CheapestTier(route provider.Route, _ provider.FundingTier) (int, bool) {
	return f[route.DeploymentID], true
}

func routedWithLatency(t *testing.T, funding FundingRanker, latency LatencyReader) []contract.DeploymentID {
	t.Helper()
	line := capabilityLine(t, `["tools"]`, `["tools"]`)
	resolved, failure := resolveGrant(capabilityGrant(), line, funding, latency, nil, requestNeeds{}, time.Now())
	if failure != nil {
		t.Fatalf("resolveGrant refused: %v", failure)
	}
	var deployments []contract.DeploymentID
	for _, route := range resolved.routes {
		deployments = append(deployments, route.DeploymentID)
	}
	return deployments
}

// TestRecentLatencyOnlyBreaksTiesWithinAFundingTier: the faster deployment goes
// first among equals, an unobserved one is tried before an observed one, near
// equal speeds keep inventory order, and nothing is ever removed or moved
// across a funding tier (ADR 0008, decision 3).
func TestRecentLatencyOnlyBreaksTiesWithinAFundingTier(t *testing.T) {
	// Inventory order is dep_refuses, dep_silent, dep_accepts.
	slowFirst := fixedLatency{"dep_refuses": 4 * time.Second, "dep_silent": 300 * time.Millisecond, "dep_accepts": 900 * time.Millisecond}
	if got := routedWithLatency(t, nil, slowFirst); !slices.Equal(got, []contract.DeploymentID{"dep_silent", "dep_accepts", "dep_refuses"}) {
		t.Errorf("faster deployments were not first: %v", got)
	}
	// Control: with no latency evidence the order is the inventory's.
	if got := routedWithLatency(t, nil, nil); !slices.Equal(got, []contract.DeploymentID{"dep_refuses", "dep_silent", "dep_accepts"}) {
		t.Errorf("no evidence changed the order: %v", got)
	}
	// Unknown is not slow: the unobserved deployment is tried first.
	if got := routedWithLatency(t, nil, fixedLatency{"dep_refuses": 50 * time.Millisecond, "dep_accepts": 60 * time.Millisecond}); got[0] != "dep_silent" {
		t.Errorf("an unobserved deployment was ranked behind observed ones: %v", got)
	}
	// Within one band (here under the 100 ms floor) noise does not reorder.
	if got := routedWithLatency(t, nil, fixedLatency{"dep_refuses": 90 * time.Millisecond, "dep_silent": 40 * time.Millisecond, "dep_accepts": 10 * time.Millisecond}); !slices.Equal(got, []contract.DeploymentID{"dep_refuses", "dep_silent", "dep_accepts"}) {
		t.Errorf("near equal speeds were reordered: %v", got)
	}
	// Funding outranks speed: the fastest deployment on paid capacity stays behind.
	funding := tierByDeployment{"dep_refuses": 0, "dep_silent": 2, "dep_accepts": 0}
	if got := routedWithLatency(t, funding, fixedLatency{"dep_refuses": 4 * time.Second, "dep_silent": 10 * time.Millisecond, "dep_accepts": 900 * time.Millisecond}); !slices.Equal(got, []contract.DeploymentID{"dep_accepts", "dep_refuses", "dep_silent"}) {
		t.Errorf("latency crossed a funding tier: %v", got)
	}
}

// windowLine serves one line from a small-window deployment, one that states
// nothing, and a large-window one, in that inventory order.
func windowLine(t *testing.T) *inventory.Inventory {
	t.Helper()
	deployment := func(id, slug, observed string) map[string]any {
		entry := map[string]any{
			"deploymentId": id, "provider": slug, "current": true,
			"modelReference": "acme/chat@observed-2026-11-12", "upstreamModelId": "chat",
		}
		if observed != "" {
			entry["observed"] = json.RawMessage(observed)
		}
		return entry
	}
	document, err := json.Marshal(map[string]any{
		"snapshotId": "snap_window", "issuedAt": contract.NewTimestamp(time.Now()),
		"deployments": []map[string]any{
			deployment("dep_small", "alpha", `{"contextTokens":8000,"maxOutputTokens":1000}`),
			deployment("dep_silent", "beta", ""),
			deployment("dep_large", "gamma", `{"contextTokens":200000,"maxOutputTokens":64000}`),
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

// TestAGrantSkipsADeploymentWhoseWindowTheRequestCannotFit: an input past a
// deployment's context window, or an output limit above its own, skips it while
// another route can serve; a deployment that states no window is kept.
func TestAGrantSkipsADeploymentWhoseWindowTheRequestCannotFit(t *testing.T) {
	line := windowLine(t)
	output := 5000
	for name, c := range map[string]struct {
		needs requestNeeds
		want  []contract.DeploymentID
	}{
		"a small request fits everywhere": {requestNeeds{inputTokens: 500}, []contract.DeploymentID{"dep_small", "dep_silent", "dep_large"}},
		"an input past the small window":  {requestNeeds{inputTokens: 50000}, []contract.DeploymentID{"dep_silent", "dep_large"}},
		"an output past the small limit":  {requestNeeds{inputTokens: 500, maxOutputTokens: &output}, []contract.DeploymentID{"dep_silent", "dep_large"}},
		"exactly the window still fits":   {requestNeeds{inputTokens: 8000}, []contract.DeploymentID{"dep_small", "dep_silent", "dep_large"}},
	} {
		resolved, failure := resolveGrant(capabilityGrant(), line, nil, nil, nil, c.needs, time.Now())
		if failure != nil {
			t.Fatalf("%s: %v", name, failure)
		}
		var got []contract.DeploymentID
		for _, route := range resolved.routes {
			got = append(got, route.DeploymentID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: routed to %v, want %v", name, got, c.want)
		}
	}
	// Every admitted deployment too small: the request still reaches an attempt.
	grant := capabilityGrant()
	grant.Steps[0].Limits.ProviderAllowlist = []contract.ProviderSlug{"alpha"}
	if resolved, failure := resolveGrant(grant, line, nil, nil, nil, requestNeeds{inputTokens: 50000}, time.Now()); failure != nil || len(resolved.routes) != 1 {
		t.Errorf("a request no admitted window fits was turned into %v instead of reaching the attempt", failure)
	}
}
