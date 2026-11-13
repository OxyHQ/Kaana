package publicapi

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestNativeTariffVariantsPreserveFundingAndCallerPreferences(t *testing.T) {
	current := capabilityLine(t, `["tools"]`, `["tools"]`)
	ids := []contract.DeploymentID{"dep_refuses", "dep_silent", "dep_accepts"}
	var steps []contract.GrantStep
	var prices nativeLinePrices
	for i, id := range ids {
		step := capabilityGrant().Steps[0]
		step.Limits.AdmittedModelLines = []contract.AdmittedModelLine{{ModelLine: "acme/chat", PriceVersionID: string(id)}}
		steps = append(steps, step)
		prices = append(prices, nativeadmission.ModelPrice{ModelLine: "acme/chat", PriceVersionID: string(id), Currency: "USD", UnitPrices: []nativeadmission.UnitPrice{{Unit: contract.UnitInputTokens, Amount: []string{"1", "2", "3"}[i], Per: 1000000, Currency: "USD"}, {Unit: contract.UnitOutputTokens, Amount: "1", Per: 1000000, Currency: "USD"}}})
	}
	authorization := routeAuthorization{requestID: "native-price-variants", steps: steps, groups: []int{0, 0, 0}, permits: func(step int, route provider.Route) bool { return route.DeploymentID == ids[step] }}
	funding := tierByDeployment{"dep_refuses": 2, "dep_silent": 0, "dep_accepts": 1}
	resolved, failure := resolveAuthorizedFor(authorization, current, funding, nil, nil, requestNeeds{prices: prices}, time.Now())
	if failure != nil || len(resolved.routes) != 3 || resolved.routes[0].DeploymentID != "dep_silent" || resolved.routes[1].DeploymentID != "dep_accepts" {
		t.Fatal("tariff split changed free/credits/paid ordering", failure)
	}
	ceiling := 1.5
	resolved, failure = resolveAuthorizedFor(authorization, current, funding, nil, nil, requestNeeds{prices: prices, prefs: &providerPreferences{MaxPrice: &maxPrice{Prompt: &ceiling}}}, time.Now())
	if failure != nil || len(resolved.routes) != 1 || resolved.routes[0].DeploymentID != "dep_refuses" {
		t.Fatal("caller ceiling used another provider tariff", failure)
	}
	disabled := false
	for i := range authorization.steps {
		authorization.steps[i].Limits.SameModelFailover = &disabled
	}
	resolved, failure = resolveAuthorizedFor(authorization, current, funding, nil, nil, requestNeeds{prices: prices}, time.Now())
	if failure != nil || len(resolved.routes) != 1 || resolved.routes[0].DeploymentID != "dep_silent" {
		t.Fatal("same-model failover crossed tariff variants", failure)
	}
}
func TestNativeScopedCacheWriteAndCatalogueNeverBorrowAnotherTariff(t *testing.T) {
	first := nativeadmission.RoutePrice{Route: nativeadmission.RoutePermission{DeploymentID: "first", Provider: "alpha", ModelReference: "acme/chat@v1"}, Price: nativeadmission.ModelPrice{ModelLine: "acme/chat", PriceVersionID: "first-price", Currency: "USD", UnitPrices: []nativeadmission.UnitPrice{{Unit: contract.UnitInputTokens, Amount: "1", Per: 1000000, Currency: "USD"}}}}
	second := first
	second.Route.DeploymentID = "second"
	second.Route.Provider = "beta"
	second.Price.PriceVersionID = "second-price"
	second.Price.UnitPrices = append(slices.Clone(first.Price.UnitPrices), nativeadmission.UnitPrice{Unit: contract.UnitCacheWriteInputTokens, Amount: "1.25", Per: 1000000, Currency: "USD"})
	a := &admission{native: &nativeadmission.Admission{Plan: nativeadmission.Plan{Prices: []nativeadmission.ModelPrice{first.Price, second.Price}, RoutePrices: []nativeadmission.RoutePrice{first, second}}}}
	server := &Server{}
	priced := server.cacheWritePricedAdmission(a)
	for _, entry := range []nativeadmission.RoutePrice{first, second} {
		want := entry.Route.DeploymentID == "second"
		if got := priced(provider.Route{DeploymentID: entry.Route.DeploymentID, Provider: entry.Route.Provider, ModelReference: entry.Route.ModelReference}); got != want {
			t.Fatal("cache writes used line-level tariff")
		}
	}
	if priced(provider.Route{DeploymentID: "second", Provider: "alpha", ModelReference: "acme/chat@v1"}) {
		t.Fatal("cross-provider cache permission")
	}
	original := declaredModel{fields: map[string]json.RawMessage{"pricing": json.RawMessage(`{"priceVersionId":"global"}`)}}
	many := nativePricedDeclaration(original, "acme/chat", []nativeadmission.RoutePrice{first, second})
	if _, ok := many.fields["pricing"]; ok {
		t.Fatal("invented aggregate provider price")
	}
	single := nativePricedDeclaration(original, "acme/chat", []nativeadmission.RoutePrice{first})
	var actual contract.PriceSnapshot
	if err := json.Unmarshal(single.fields["pricing"], &actual); err != nil || actual.PriceVersionID != "first-price" {
		t.Fatal("native model shows global tariff", err)
	}
	if string(original.fields["pricing"]) != `{"priceVersionId":"global"}` {
		t.Fatal("global catalogue mutated")
	}
}
