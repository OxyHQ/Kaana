package nativeadmission

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func scopedFixture() (Policy, Model) {
	p := policy()
	m := model("medium")
	second := RoutePermission{DeploymentID: "deployment-second", ModelReference: "fixture/medium@immutable", Provider: "second"}
	p.Routes.Deployments = append(p.Routes.Deployments, second)
	for index, route := range p.Routes.Deployments {
		price := price(m.ID)
		price.PriceVersionID = string(route.DeploymentID) + "-price"
		if index == 1 {
			price.UnitPrices[0].Amount = "3"
			price.UnitPrices[3].Amount = "4"
			price.UnitPrices[4].Amount = "4"
		}
		p.PriceBook = append(p.PriceBook, RoutePrice{Route: route, Price: price, SourceArtifactSHA256: strings.Repeat("a", 64), SourcePriceVersionID: "previous-" + price.PriceVersionID, SourceEvidenceRef: "reviewed:snapshot"})
	}
	m.Price = ModelPrice{}
	return p, m
}
func TestScopedTariffsAdmitAndRateOnlyTheServedTuple(t *testing.T) {
	p, m := scopedFixture()
	planned, err := planner(t, p, m).Plan(principal(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Steps) != 2 || len(planned.RoutePrices) != 2 || len(planned.StepGroups) != 2 || planned.StepGroups[0] != planned.StepGroups[1] {
		t.Fatal("tariff variants lost shared priority")
	}
	for i, entry := range p.PriceBook {
		report := contract.UsageReport{ResolvedModelReference: entry.Route.ModelReference, ServingProvider: entry.Route.Provider, DeploymentID: entry.Route.DeploymentID, Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 1000}, {Unit: contract.UnitOutputTokens, Quantity: 100}}}
		price, err := planned.PriceForReport(report)
		if err != nil || price.PriceVersionID != entry.Price.PriceVersionID {
			t.Fatal("wrong served tariff", err)
		}
		amount, err := RateUnits(price, report.Units)
		want := []string{"0.001200000000", "0.003400000000"}[i]
		if err != nil || string(amount.Amount) != want {
			t.Fatal("wrong exact rating", amount, err)
		}
		for _, mutate := range []func(*contract.UsageReport){func(r *contract.UsageReport) { r.DeploymentID = "" }, func(r *contract.UsageReport) { r.ServingProvider = "other" }, func(r *contract.UsageReport) { r.ResolvedModelReference = "fixture/medium@other" }} {
			changed := report
			mutate(&changed)
			if _, err = planned.PriceForReport(changed); !errors.Is(err, ErrUnpriced) {
				t.Fatal("ambiguous tuple selected tariff")
			}
		}
	}
	if m.Price.PriceVersionID != "" {
		t.Fatal("declared catalogue mutated")
	}
	// A route over the request ceiling is excluded independently, without losing
	// an affordable provider of the same model or using its price for the other.
	p.Limits.MaxPricePerRequest = &contract.Money{Amount: "0.02", Currency: "USD"}
	planned, err = planner(t, p, m).Plan(principal(), request())
	if err != nil || len(planned.RoutePrices) != 1 || planned.RoutePrices[0].Route.Provider != "fixture" {
		t.Fatal("per-route quote bound not applied", err)
	}
	if planned.StepRoutes[0].Allows("deployment-second", "fixture/medium@immutable", "second", true) {
		t.Fatal("expensive route inherited cheaper tariff")
	}
}
func TestScopedAdmissionRejectsChangedSnapshot(t *testing.T) {
	p, m := scopedFixture()
	planned, err := planner(t, p, m).Plan(principal(), request())
	if err != nil {
		t.Fatal(err)
	}
	if !p.admitsExecution(contract.GrantResponses, planned) {
		t.Fatal("valid snapshot refused")
	}
	for _, mutate := range []func(*Plan){func(plan *Plan) { plan.RoutePrices[0].Price.UnitPrices[0].Amount = "0" }, func(plan *Plan) { plan.RoutePrices[0].Route.Provider = "other" }, func(plan *Plan) { plan.RoutePrices = nil }, func(plan *Plan) { plan.Steps[0].Limits.AdmittedModelLines[0].PriceVersionID = "unreviewed" }} {
		changed := clone(planned)
		mutate(&changed)
		if p.admitsExecution(contract.GrantResponses, changed) {
			t.Fatal("changed immutable tariff accepted")
		}
	}
}
func TestPostgresScopedSettlementUsesExecutedProviderAfterReplay(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p, _ := scopedFixture()
	enableTestPolicy(t, s, pool, ctx, p)
	for index, entry := range p.PriceBook {
		key := contract.IdempotencyKey("scoped-" + string(entry.Route.Provider))
		request := storeRequest(t, p, "scoped-"+string(entry.Route.Provider), &key)
		a, err := s.Admit(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		report := reportFor(a)
		report.DeploymentID = entry.Route.DeploymentID
		report.ServingProvider = entry.Route.Provider
		report.ResolvedModelReference = entry.Route.ModelReference
		report.Units = []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 1000}, {Unit: contract.UnitOutputTokens, Quantity: 100}}
		if err = s.StageCompletion(ctx, a.ID, report, nil); err != nil {
			t.Fatal(err)
		}
		fresh := NewStore(pool)
		if count, err := fresh.ReplayCompletions(ctx, 10); err != nil || count != 1 {
			t.Fatal("durable replay", count, err)
		}
		raw, err := fresh.Generation(ctx, a.Principal, nil, &key, nil)
		if err != nil {
			t.Fatal(err)
		}
		var record contract.MeteredGenerationRecord
		if err = json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		want := []string{"0.001200000000", "0.003400000000"}[index]
		if record.Tariff.Amount == nil || string(*record.Tariff.Amount) != want || record.Tariff.PriceVersionID == nil || *record.Tariff.PriceVersionID != entry.Price.PriceVersionID {
			t.Fatal("served tariff identity lost on restart")
		}
	}
}
