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
	projected, err := catalogue.nativeModels(current, time.Now())
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
