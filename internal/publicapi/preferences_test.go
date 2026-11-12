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

// preferenceLine serves one line from four deployments in inventory order:
// alpha (fp8, takes temperature), beta (no statements at all), gamma (fp4, no
// temperature) and delta (bf16, takes temperature).
func preferenceLine(t *testing.T) *inventory.Inventory {
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
		"snapshotId": "snap_preferences", "issuedAt": contract.NewTimestamp(time.Now()),
		"deployments": []map[string]any{
			deployment("dep_alpha", "alpha", `{"quantizations":["fp8"],"acceptedParameters":["sampling.temperature","tools"]}`),
			deployment("dep_beta", "beta", ""),
			deployment("dep_gamma", "gamma", `{"quantizations":["fp4"],"acceptedParameters":["tools"]}`),
			deployment("dep_delta", "delta", `{"quantizations":["bf16"],"acceptedParameters":["sampling.temperature","tools"]}`),
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

type fixedPerformance struct {
	first      map[contract.DeploymentID]time.Duration
	throughput map[contract.DeploymentID]float64
}

func (f fixedPerformance) FirstOutput(id contract.DeploymentID) (time.Duration, bool) {
	d, ok := f.first[id]
	return d, ok
}

func (f fixedPerformance) Throughput(id contract.DeploymentID) (float64, bool) {
	v, ok := f.throughput[id]
	return v, ok
}

func routedBy(t *testing.T, needs requestNeeds, performance PerformanceReader) ([]contract.DeploymentID, *contract.PublicError) {
	t.Helper()
	resolved, failure := resolveGrant(capabilityGrant(), preferenceLine(t), nil, performance, nil, needs, time.Now())
	if failure != nil {
		return nil, failure
	}
	var deployments []contract.DeploymentID
	for _, route := range resolved.routes {
		deployments = append(deployments, route.DeploymentID)
	}
	return deployments, nil
}

func prefs(t *testing.T, document string) *providerPreferences {
	t.Helper()
	var decoded providerPreferences
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatal(err)
	}
	if failure := decoded.validate("req"); failure != nil {
		t.Fatalf("%s was refused: %v", document, failure)
	}
	return &decoded
}

func assertRoutes(t *testing.T, name string, got []contract.DeploymentID, want ...contract.DeploymentID) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: routed to %v, want %v", name, got, want)
	}
}

// TestProviderPreferencesOnlyNarrowWhatTheGrantAdmitted: every control either
// removes routes or reorders them; none adds a deployment, and a request they
// leave with no route is refused rather than widened.
func TestProviderPreferencesOnlyNarrowWhatTheGrantAdmitted(t *testing.T) {
	all := []contract.DeploymentID{"dep_alpha", "dep_beta", "dep_gamma", "dep_delta"}
	got, _ := routedBy(t, requestNeeds{}, nil)
	assertRoutes(t, "control: no preference", got, all...)

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"only":["gamma","delta"]}`)}, nil)
	assertRoutes(t, "only", got, "dep_gamma", "dep_delta")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"ignore":["alpha","delta"]}`)}, nil)
	assertRoutes(t, "ignore", got, "dep_beta", "dep_gamma")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"order":["delta","gamma"]}`)}, nil)
	assertRoutes(t, "order puts the listed first and keeps the rest behind them", got, "dep_delta", "dep_gamma", "dep_alpha", "dep_beta")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"order":["delta","gamma"],"allow_fallbacks":false}`)}, nil)
	assertRoutes(t, "order without fallbacks", got, "dep_delta", "dep_gamma")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"allow_fallbacks":false}`)}, nil)
	assertRoutes(t, "no fallbacks keeps the best route only", got, "dep_alpha")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"quantizations":["fp8","bf16"]}`)}, nil)
	assertRoutes(t, "quantizations", got, "dep_alpha", "dep_delta")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"quantizations":["unknown"]}`)}, nil)
	assertRoutes(t, "a route that states no quantization is unknown", got, "dep_beta")

	// A provider the grant does not admit cannot be asked for.
	grant := capabilityGrant()
	grant.Steps[0].Limits.ProviderDenylist = []contract.ProviderSlug{"gamma"}
	resolved, failure := resolveGrant(grant, preferenceLine(t), nil, nil, nil, requestNeeds{prefs: prefs(t, `{"only":["gamma"]}`)}, time.Now())
	if failure == nil || resolved != nil {
		t.Errorf("a preference widened past the grant's own provider limit: %v", resolved)
	}
	if _, failure := routedBy(t, requestNeeds{prefs: prefs(t, `{"only":["nobody"]}`)}, nil); failure == nil || failure.Code != "no_route_available" {
		t.Errorf("a request the preference left with no route was %v, want no_route_available", failure)
	}
}

// TestRequireParametersIsHardWhileTheDefaultIsAPreference: with the flag set a
// route that refuses a control is removed even if it is the only one left;
// without it the request still reaches an attempt (ADR 0008, decision 1).
func TestRequireParametersIsHardWhileTheDefaultIsAPreference(t *testing.T) {
	temperature := []provider.RequestParameter{provider.ParameterTemperature}

	got, _ := routedBy(t, requestNeeds{parameters: temperature}, nil)
	assertRoutes(t, "default: routes that accept it first", got, "dep_alpha", "dep_beta", "dep_delta")

	got, _ = routedBy(t, requestNeeds{parameters: temperature, prefs: prefs(t, `{"require_parameters":true}`)}, nil)
	assertRoutes(t, "require_parameters", got, "dep_alpha", "dep_beta", "dep_delta")

	only := prefs(t, `{"only":["gamma"],"require_parameters":true}`)
	if _, failure := routedBy(t, requestNeeds{parameters: temperature, prefs: only}, nil); failure == nil {
		t.Error("require_parameters let a route that refuses the control through")
	}
	soft := prefs(t, `{"only":["gamma"]}`)
	if got, failure := routedBy(t, requestNeeds{parameters: temperature, prefs: soft}, nil); failure != nil || !slices.Equal(got, []contract.DeploymentID{"dep_gamma"}) {
		t.Errorf("without require_parameters the attempt must still be reached: %v %v", got, failure)
	}
}

// TestSortAndPerformanceFloorsReorderButNeverExclude.
func TestSortAndPerformanceFloorsReorderButNeverExclude(t *testing.T) {
	performance := fixedPerformance{
		first: map[contract.DeploymentID]time.Duration{
			"dep_alpha": 4 * time.Second, "dep_beta": 300 * time.Millisecond, "dep_gamma": 900 * time.Millisecond, "dep_delta": 2 * time.Second},
		throughput: map[contract.DeploymentID]float64{"dep_alpha": 20, "dep_beta": 60, "dep_gamma": 400, "dep_delta": 120},
	}
	got, _ := routedBy(t, requestNeeds{prefs: prefs(t, `{"sort":"latency"}`)}, performance)
	assertRoutes(t, "sort by latency", got, "dep_beta", "dep_gamma", "dep_delta", "dep_alpha")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"sort":"throughput"}`)}, performance)
	assertRoutes(t, "sort by throughput", got, "dep_gamma", "dep_delta", "dep_beta", "dep_alpha")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"preferred_max_latency":1}`)}, performance)
	assertRoutes(t, "a latency floor demotes the slow ones, keeps them (and they keep their own latency order)", got, "dep_beta", "dep_gamma", "dep_delta", "dep_alpha")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"preferred_min_throughput":100}`)}, performance)
	assertRoutes(t, "a throughput floor demotes the slow ones, keeps them (and they keep their own latency order)", got, "dep_gamma", "dep_delta", "dep_beta", "dep_alpha")

	got, _ = routedBy(t, requestNeeds{prefs: prefs(t, `{"preferred_max_latency":0.001}`)}, performance)
	if len(got) != 4 {
		t.Errorf("a floor nothing meets removed routes: %v", got)
	}
}

// TestACacheKeyKeepsARequestOnItsDeploymentButNeverOverridesAnExplicitChoice.
func TestACacheKeyKeepsARequestOnItsDeploymentButNeverOverridesAnExplicitChoice(t *testing.T) {
	got, _ := routedBy(t, requestNeeds{affine: "dep_gamma"}, nil)
	assertRoutes(t, "affinity moves its deployment to the front", got, "dep_gamma", "dep_alpha", "dep_beta", "dep_delta")

	got, _ = routedBy(t, requestNeeds{affine: "dep_gamma", prefs: prefs(t, `{"order":["delta"]}`)}, nil)
	assertRoutes(t, "the caller's explicit order outranks it", got, "dep_delta", "dep_gamma", "dep_alpha", "dep_beta")

	got, _ = routedBy(t, requestNeeds{affine: "dep_gamma", prefs: prefs(t, `{"ignore":["gamma"]}`)}, nil)
	assertRoutes(t, "a deployment that is no longer admitted is not resurrected", got, "dep_alpha", "dep_beta", "dep_delta")
}

func TestAffinityTableIsScopedBoundedAndExpires(t *testing.T) {
	grantFor := func(account string) *contract.Grant {
		grant := capabilityGrant()
		grant.Principal.BillingAccountID = contract.AccountID(account)
		return grant
	}
	table := newAffinityTable()
	now := time.Now()
	mine, theirs := affinityKey(grantFor("acct_a"), "conversation-1"), affinityKey(grantFor("acct_b"), "conversation-1")
	if mine == theirs {
		t.Fatal("two accounts sending the same key share an affinity entry")
	}
	table.set(mine, "dep_alpha", now)
	if got, ok := table.get(mine, now.Add(time.Minute)); !ok || got != "dep_alpha" {
		t.Errorf("recorded affinity = %q %v", got, ok)
	}
	if _, ok := table.get(theirs, now); ok {
		t.Error("another account read this account's affinity")
	}
	if _, ok := table.get(mine, now.Add(affinityTTL+time.Second)); ok {
		t.Error("an expired affinity was still used")
	}
	for i := range maxAffinityEntries + 50 {
		table.set(affinityKey(grantFor("acct_a"), string(rune('a'+i%26))+time.Duration(i).String()), "dep_alpha", now.Add(time.Duration(i)))
	}
	if len(table.entries) > maxAffinityEntries {
		t.Errorf("the table grew to %d entries", len(table.entries))
	}
}

func TestProviderPreferencesValidation(t *testing.T) {
	for name, document := range map[string]string{
		"an empty only":          `{"only":[]}`,
		"a slug with a capital":  `{"order":["Alpha"]}`,
		"seventeen providers":    `{"ignore":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]}`,
		"a made up quantization": `{"quantizations":["fp3"]}`,
		"sorting by price":       `{"sort":"price"}`,
		"a negative latency":     `{"preferred_max_latency":-1}`,
		"a zero throughput":      `{"preferred_min_throughput":0}`,
	} {
		var decoded providerPreferences
		if err := json.Unmarshal([]byte(document), &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if failure := decoded.validate("req"); failure == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if failure := (*providerPreferences)(nil).validate("req"); failure != nil {
		t.Errorf("no preference was refused: %v", failure)
	}
}
