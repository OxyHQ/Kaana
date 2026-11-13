package publicapi

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestNativeProjectionRetainsInternalCapabilitiesAndImageBound(t *testing.T) {
	catalogue, err := LoadPublicCatalogue("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var declared struct {
		Models []struct {
			ModelID    contract.ModelID `json:"modelId"`
			ImageInput *struct {
				MaxInputTokensPerImage int `json:"maxInputTokensPerImage"`
			} `json:"imageInput"`
		}
	}
	if err = json.Unmarshal(catalogue.Document(), &declared); err != nil {
		t.Fatal(err)
	}
	var imageID contract.ModelID
	var imageBound int
	for _, entry := range declared.Models {
		if entry.ImageInput != nil {
			imageID = entry.ModelID
			imageBound = entry.ImageInput.MaxInputTokensPerImage
			break
		}
	}
	if imageID == "" || imageBound <= 0 {
		t.Fatal("positive image declaration missing")
	}
	const textID = contract.ModelID("deepseek/deepseek-v4-flash")
	raw, err := json.Marshal(map[string]any{"snapshotId": "snap_native_projection", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []map[string]any{
		{"deploymentId": "dep_native_text", "provider": "test", "current": true, "modelReference": string(textID) + "@fixture", "upstreamModelId": "text", "observed": map[string]any{"acceptedParameters": []string{"responseFormat"}}},
		{"deploymentId": "dep_native_image", "provider": "test", "current": true, "modelReference": string(imageID) + "@fixture", "upstreamModelId": "image"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := inventory.Parse(raw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := catalogue.nativeModels(current, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 2 {
		t.Fatalf("projected %d served lines", len(projected))
	}
	for _, model := range projected {
		if model.Price.PriceVersionID == "" || len(model.Price.UnitPrices) == 0 {
			t.Fatal("projection lost exact declared tariff")
		}
		switch model.ID {
		case imageID:
			if model.MaxInputTokensPerImage != imageBound {
				t.Fatal("internal image token ceiling was lost")
			}
		case textID:
			if model.StructuredOutput || !slices.Contains(model.Unreported, "structuredOutput") || !slices.Contains(model.AcceptedParameters, provider.ParameterResponseFormat) {
				t.Fatal("unknown capability was invented or live evidence was lost")
			}
		default:
			t.Fatal("unexpected model")
		}
	}
}

func TestNativeRoutePermissionPrecedesDedupAndKeepsHardNeeds(t *testing.T) {
	current := capabilityLine(t, `[]`, `["sampling.temperature"]`)
	grant := capabilityGrant()
	authorization := routeAuthorization{requestID: grant.RequestID, steps: append(grant.Steps, grant.Steps...), permits: func(step int, route provider.Route) bool { return step == 1 && route.DeploymentID == "dep_accepts" }}
	resolved, failure := resolveAuthorizedFor(authorization, current, nil, nil, nil, requestNeeds{parameters: []provider.RequestParameter{provider.ParameterTemperature}}, time.Now())
	if failure != nil || len(resolved.routes) != 1 || *resolved.stepOf("dep_accepts") != 1 {
		t.Fatalf("wrong step claimed route: %v", failure)
	}
	authorization.permits = func(_ int, route provider.Route) bool { return route.DeploymentID == "dep_refuses" }
	if _, failure = resolveAuthorizedFor(authorization, current, nil, nil, nil, requestNeeds{parameters: []provider.RequestParameter{provider.ParameterTemperature}}, time.Now()); failure == nil {
		t.Fatal("native route silently dropped requested capability")
	}
}

func TestReviewedReasoningOutputFitsDeclaredTariffAndContext(t *testing.T) {
	catalogue, err := LoadPublicCatalogue("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	const line contract.ModelID = "deepseek/deepseek-v4-flash-0731"
	const reference contract.ModelReference = "deepseek/deepseek-v4-flash-0731@observed-2026-09-01"
	raw, _ := json.Marshal(map[string]any{"snapshotId": "native-reviewed-price", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []map[string]any{{"deploymentId": "dep_openrouter_deepseek_deepseek_v4_flash_0731_observed_2026_09_01", "provider": "openrouter", "modelReference": reference, "upstreamModelId": string(line), "current": true, "observed": map[string]any{"acceptedParameters": []string{"maxOutputTokens", "responseFormat"}}}}})
	current, err := inventory.Parse(raw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	models, err := catalogue.nativeModels(current, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := contract.AccountID("reviewed-owner")
	principal := contract.IdentityPrincipal{OwnerAccountID: &owner, ApplicationID: "reviewed-app", CredentialID: "reviewed-workload", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.IdentityAuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}
	tier := contract.FundingTierPaid
	p := nativeadmission.Policy{ID: "reviewed-output-budget", Version: 1, Enabled: true, CredentialIDs: []contract.CredentialID{principal.CredentialID}, OwnerAccountID: owner, ApplicationID: principal.ApplicationID, Environment: principal.Environment, Lane: principal.Lane, Authority: contract.AuthorityFirstParty, ApplicationClass: principal.ApplicationClass, ApplicationIsInternal: true, Audience: contract.AudiencePlatformInternal, AllowedLevels: []contract.PowerLevel{contract.PowerAuto, contract.PowerHigh, contract.PowerXHigh}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: "dep_openrouter_deepseek_deepseek_v4_flash_0731_observed_2026_09_01", ModelReference: reference, Provider: "openrouter"}}}, Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.05", Currency: "USD"}, MaxFundingTier: &tier, RequireZeroDataRetention: true, ProhibitTrainingOnCustomerData: true}, MaxInputTokens: 126976, MaxOutputTokens: 8192, DefaultOutputTokens: 8192, MaxTools: 128, MaxConcurrent: 32, MaxPerUTCDay: 5000, ExecutionTimeout: time.Minute}
	planner, err := nativeadmission.NewPlanner([]nativeadmission.Policy{p}, models)
	if err != nil {
		t.Fatal(err)
	}
	target := contract.LevelTarget(contract.PowerAuto)
	input, output := 126976, 8192
	request := nativeadmission.Request{Operation: contract.GrantResponses, Target: &target, Facts: contract.GrantRequestFacts{EstimatedInputTokens: &input, MaxOutputTokens: &output}, StructuredOutput: true}
	plan, err := planner.Plan(principal, request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Quote.Amount != "0.007274496000" || plan.OutputTokens != 8192 || len(plan.Prices) != 1 || plan.Prices[0].ModelLine != line {
		t.Fatalf("reviewed tariff/budget changed: %+v", plan.Quote)
	}
	if !plan.AllowsParameters(line, current.Catalogue()[0].AcceptedParameters) {
		t.Fatal("actual schema route evidence was lost")
	}
	p.Limits.MaxPricePerRequest = &contract.Money{Amount: "0.007", Currency: "USD"}
	planner, err = nativeadmission.NewPlanner([]nativeadmission.Policy{p}, models)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = planner.Plan(principal, request); !errors.Is(err, nativeadmission.ErrNoRoute) {
		t.Fatal("price bound did not refuse larger output")
	}
	p.Limits.MaxPricePerRequest = &contract.Money{Amount: "0.05", Currency: "USD"}
	models[0].MaxContextTokens = input + output - 1
	planner, err = nativeadmission.NewPlanner([]nativeadmission.Policy{p}, models)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = planner.Plan(principal, request); !errors.Is(err, nativeadmission.ErrNoRoute) {
		t.Fatal("context bound did not refuse full allowance")
	}
}
