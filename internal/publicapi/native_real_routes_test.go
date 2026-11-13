package publicapi

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/commercial"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

func reviewedNativeRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	var adapters []provider.Registrant
	for _, slug := range []contract.ProviderSlug{"cerebras", "groq", "openrouter", "xai"} {
		endpoint := providerconfig.Known[slug]
		adapter, err := openaicompat.New(openaicompat.Config{Provider: slug, BaseURL: endpoint.BaseURL})
		if err != nil {
			t.Fatal(err)
		}
		adapters = append(adapters, adapter)
	}
	registry, err := provider.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestReviewedNativeExactRoutesUseRealAdapterDialects(t *testing.T) {
	registry := reviewedNativeRegistry(t)
	catalogue, err := LoadPublicCatalogue("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := nativeadmission.LoadPolicyFile("../../configs/native-admission/alia-kaana-v2.json", "eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490")
	if err != nil {
		t.Fatal(err)
	}
	p := policies[0]
	principal := contract.IdentityPrincipal{OwnerAccountID: &p.OwnerAccountID, ApplicationID: p.ApplicationID, CredentialID: p.CredentialIDs[0], Environment: p.Environment, Lane: p.Lane, Authority: contract.IdentityAuthority(p.Authority), ApplicationClass: p.ApplicationClass, ApplicationIsInternal: p.ApplicationIsInternal}
	declarations, err := commercial.Load("../../configs/deployment-commercial.json")
	if err != nil {
		t.Fatal(err)
	}
	for group, scope := range []nativeadmission.RouteScope{p.Routes, p.ImageInput.Routes, p.Speech.Routes} {
		for _, permission := range scope.Deployments {
			t.Run(string(permission.DeploymentID), func(t *testing.T) {
				upstream := string(permission.ModelReference.ModelID())
				if group == 2 {
					upstream = "tts"
				}
				parameters := []string{"maxOutputTokens", "responseFormat", "tools"}
				raw, _ := json.Marshal(map[string]any{"snapshotId": "native-real-adapter-fixture", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []map[string]any{{"deploymentId": permission.DeploymentID, "provider": permission.Provider, "modelReference": permission.ModelReference, "upstreamModelId": upstream, "current": true, "observed": map[string]any{"acceptedParameters": parameters}}}})
				current, err := inventory.Parse(raw, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				routes, err := current.Resolve(permission.ModelReference, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				route := routes.Candidates()[0]
				target := contract.ModelTarget(permission.ModelReference)
				input, output := 128, 16384
				request := nativeadmission.Request{Operation: contract.GrantResponses, Target: &target, Facts: contract.GrantRequestFacts{EstimatedInputTokens: &input, MaxOutputTokens: &output, DeclaredTools: 1}, StructuredOutput: true}
				if group == 1 {
					images := 1
					request.Facts.InputImages = &images
				}
				if group == 2 {
					request.Operation = contract.GrantAudioSpeech
					request.Facts = contract.GrantRequestFacts{InputCharacters: &input, Stream: true}
					request.StructuredOutput = false
				}
				if !nativeOperationServed(registry, registry, route, request.Operation, nil) || !nativeCommercialAllows(declarations, principal, route) {
					t.Fatal("reviewed route refused by adapter/commercial bridge")
				}
				models, err := catalogue.nativeModels(current, time.Now(), registry)
				if err != nil {
					t.Fatal(err)
				}
				planner, err := nativeadmission.NewPlanner(policies, models)
				if err != nil {
					t.Fatal(err)
				}
				planned, err := planner.Plan(principal, request)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(planned.StepRoutes, func(scope nativeadmission.RouteScope) bool {
					return scope.Allows(route.DeploymentID, route.ModelReference, route.Provider, false)
				}) || !planned.AllowsParameters(route.ModelReference.ModelID(), route.AcceptedParameters) {
					t.Fatal("reviewed route lost permission or capability evidence")
				}
				needs := requestNeeds{prices: nativeLinePrices(planned.Prices)}
				if group != 2 {
					needs.maxOutputTokens = &planned.OutputTokens
					needs.inputTokens = planned.InputTokens
					needs.parameters = []provider.RequestParameter{provider.ParameterTools, provider.ParameterResponseFormat, provider.ParameterMaxOutputTokens}
				}
				authorization := routeAuthorization{requestID: "native-real-route", steps: planned.Steps, groups: planned.StepGroups, permits: func(step int, r provider.Route) bool {
					return nativeOperationServed(registry, registry, r, request.Operation, nil) && planned.StepRoutes[step].Allows(r.DeploymentID, r.ModelReference, r.Provider, false) && planned.AllowsParameters(r.ModelReference.ModelID(), r.AcceptedParameters)
				}}
				resolved, failure := resolveAuthorizedFor(authorization, current, nil, nil, nil, needs, time.Now())
				if failure != nil || len(resolved.routes) != 1 {
					t.Fatal("reviewed route cannot reach execution")
				}
				if group != 2 && planned.OutputTokens != 8192 {
					t.Fatal("normalized output bound lost")
				}
			})
		}
	}
}

func TestNativeTextDialectDoesNotAuthorizeOtherModelFamilies(t *testing.T) {
	registry := reviewedNativeRegistry(t)
	for _, tc := range []struct {
		slug  contract.ProviderSlug
		model string
		want  bool
	}{
		{"openrouter", "openai/gpt-oss-120b", true}, {"openrouter", "openai/gpt-5.4-nano", true}, {"xai", "grok-4", true},
		{"openrouter", "openai/gpt-audio-mini", false}, {"openrouter", "openai/text-embedding-3-small", false}, {"openrouter", "typesafe/jev", false}, {"xai", "tts", false}, {"openrouter", "openai/gpt-image-1", false}, {"groq", "meta-llama/llama-prompt-guard-2-22m", false},
	} {
		route := provider.Route{Provider: tc.slug, UpstreamModelID: tc.model}
		if got := nativeFormatServed(registry, route, contract.APIFormatResponses); got != tc.want {
			t.Fatalf("%s/%s: allowed=%t", tc.slug, tc.model, got)
		}
	}
	if nativeFormatServed(nil, provider.Route{Provider: "openrouter", UpstreamModelID: "openai/gpt-oss-120b"}, contract.APIFormatResponses) {
		t.Fatal("missing adapter registry invented capability")
	}
}
