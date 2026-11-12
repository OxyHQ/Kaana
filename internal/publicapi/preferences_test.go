package publicapi

import (
	"encoding/json"
	"math/big"
	"os"
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
		"an empty only":             `{"only":[]}`,
		"a slug with a capital":     `{"order":["Alpha"]}`,
		"seventeen providers":       `{"ignore":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]}`,
		"a made up quantization":    `{"quantizations":["fp3"]}`,
		"sorting by a made up word": `{"sort":"cheapest"}`,
		"an empty ceiling":          `{"max_price":{}}`,
		"a negative ceiling":        `{"max_price":{"prompt":-1}}`,
		"a huge ceiling":            `{"max_price":{"completion":2000000}}`,
		"a negative latency":        `{"preferred_max_latency":-1}`,
		"a zero throughput":         `{"preferred_min_throughput":0}`,
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

type fixedPrices map[contract.ModelID]linePrice

func (f fixedPrices) LinePrice(line contract.ModelID, _ string) linePrice { return f[line] }

func rat(value string) *big.Rat {
	r, _ := new(big.Rat).SetString(value)
	return r
}

// priceLines serves four model lines, one deployment each, in the order the
// grant admits them: pricey, cheap, mid and one the catalogue does not price.
func priceLines(t *testing.T) *inventory.Inventory {
	t.Helper()
	deployment := func(id, line string) map[string]any {
		return map[string]any{
			"deploymentId": id, "provider": "alpha", "current": true,
			"modelReference": line + "@observed-2026-11-12", "upstreamModelId": line,
		}
	}
	document, err := json.Marshal(map[string]any{
		"snapshotId": "snap_prices", "issuedAt": contract.NewTimestamp(time.Now()),
		"deployments": []map[string]any{
			deployment("dep_pricey", "acme/pricey"), deployment("dep_cheap", "acme/cheap"),
			deployment("dep_mid", "acme/mid"), deployment("dep_unpriced", "acme/unpriced"),
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

func priceGrant() *contract.Grant {
	level := contract.PowerXHigh
	admitted := func(line string) contract.AdmittedModelLine {
		return contract.AdmittedModelLine{ModelLine: contract.ModelID(line), PriceVersionID: "price"}
	}
	return &contract.Grant{
		RequestID: "req_prices", Operation: contract.GrantChatCompletions,
		Steps: []contract.GrantStep{{
			Target: contract.GrantTarget{Kind: contract.GrantTargetLevel, Level: &level},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{
				admitted("acme/pricey"), admitted("acme/cheap"), admitted("acme/mid"), admitted("acme/unpriced")}},
		}},
	}
}

var declaredPrices = fixedPrices{
	"acme/pricey": {Input: rat("15"), Output: rat("75")},
	"acme/cheap":  {Input: rat("0.1"), Output: rat("0.4")},
	"acme/mid":    {Input: rat("3"), Output: rat("15")},
	// acme/unpriced states nothing.
}

func routedByPrice(t *testing.T, document string, prices LinePricer) ([]contract.DeploymentID, *contract.PublicError) {
	t.Helper()
	needs := requestNeeds{prices: prices}
	if document != "" {
		needs.prefs = prefs(t, document)
	}
	resolved, failure := resolveGrant(priceGrant(), priceLines(t), nil, nil, nil, needs, time.Now())
	if failure != nil {
		return nil, failure
	}
	var deployments []contract.DeploymentID
	for _, route := range resolved.routes {
		deployments = append(deployments, route.DeploymentID)
	}
	return deployments, nil
}

// TestPriceControlsReadTheDeclaredListPriceOfTheLine: max_price removes the lines
// over a ceiling (and the ones whose price is not stated); sort price tries the
// cheapest line first, with unpriced lines last; neither widens the grant.
func TestPriceControlsReadTheDeclaredListPriceOfTheLine(t *testing.T) {
	got, _ := routedByPrice(t, "", declaredPrices)
	assertRoutes(t, "control: no preference keeps the grant's line order", got, "dep_pricey", "dep_cheap", "dep_mid", "dep_unpriced")

	got, _ = routedByPrice(t, `{"sort":"price"}`, declaredPrices)
	assertRoutes(t, "sort price", got, "dep_cheap", "dep_mid", "dep_pricey", "dep_unpriced")

	got, _ = routedByPrice(t, `{"max_price":{"prompt":5}}`, declaredPrices)
	assertRoutes(t, "a prompt ceiling drops the dearer line and the unpriced one", got, "dep_cheap", "dep_mid")

	got, _ = routedByPrice(t, `{"max_price":{"prompt":5,"completion":10}}`, declaredPrices)
	assertRoutes(t, "both components must hold", got, "dep_cheap")

	got, _ = routedByPrice(t, `{"max_price":{"completion":20},"sort":"price"}`, declaredPrices)
	assertRoutes(t, "a ceiling and a sort together", got, "dep_cheap", "dep_mid")

	got, _ = routedByPrice(t, `{"max_price":{"prompt":3}}`, declaredPrices)
	assertRoutes(t, "a ceiling equal to the price still admits it", got, "dep_cheap", "dep_mid")

	if _, failure := routedByPrice(t, `{"max_price":{"prompt":0.001}}`, declaredPrices); failure == nil || failure.Code != "no_route_available" {
		t.Errorf("a ceiling no admitted line meets was %v, want no_route_available", failure)
	}
	// Without a catalogue nothing states a price: a ceiling cannot be shown to
	// hold, so it refuses rather than serving above it.
	if _, failure := routedByPrice(t, `{"max_price":{"prompt":100}}`, nil); failure == nil {
		t.Error("a ceiling was treated as met with no price stated")
	}
	got, _ = routedByPrice(t, `{"sort":"price"}`, nil)
	assertRoutes(t, "sort price with no price stated keeps the grant's order", got, "dep_pricey", "dep_cheap", "dep_mid", "dep_unpriced")
}

// TestLinePriceReadsTheCheckedInCatalogue: the per-million figures come from the
// declared unit prices at the price version the grant admitted, in USD; any
// other version, an undeclared line or a line without pricing states nothing.
func TestLinePriceReadsTheCheckedInCatalogue(t *testing.T) {
	raw, err := os.ReadFile("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	const line, version = "google/gemini-3.7-flash", "01a0d808-5bfb-77c3-9d20-7ded31f3a5c9"

	price := catalogue.LinePrice(line, version)
	if price.Input == nil || price.Input.Cmp(rat("0.75")) != 0 || price.Output == nil || price.Output.Cmp(rat("3.75")) != 0 {
		t.Errorf("gemini-3.7-flash = %v / %v per million, want 0.75 / 3.75", price.Input, price.Output)
	}
	if blended := price.blended(); blended == nil || blended.Cmp(rat("4.5")) != 0 {
		t.Errorf("blended = %v, want 4.5", blended)
	}
	for name, stated := range map[string]linePrice{
		"another price version": catalogue.LinePrice(line, "another-version"),
		"an undeclared line":    catalogue.LinePrice("acme/nope", version),
	} {
		if stated.Input != nil || stated.Output != nil {
			t.Errorf("%s stated a price: %+v", name, stated)
		}
	}
	if stated := (*PublicCatalogue)(nil).LinePrice(line, version); stated.Input != nil {
		t.Error("a nil catalogue stated a price")
	}
}
