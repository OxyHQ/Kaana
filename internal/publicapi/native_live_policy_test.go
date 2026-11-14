package publicapi

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/commercial"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openailive"
	"github.com/OxyHQ/Kaana/internal/provider/openairealtime"
)

// The observation is a fixture, not a claim that a current provider key or live
// inventory is ready. Catalogue prices, capabilities, adapters and commercial
// declarations are the actual production inputs; no provider call is made.
func TestReviewedNativeLivePolicyUsesRealCatalogueAndAdapters(t *testing.T) {
	policies, err := nativeadmission.LoadPolicyFile("../../configs/native-admission/alia-kaana-v3.json", "bb80ef1fb91fc589d75c99446631c18e53100031de9c68c90e17d64776e6249b")
	if err != nil {
		t.Fatal(err)
	}
	p := policies[0]
	principal := contract.IdentityPrincipal{OwnerAccountID: &p.OwnerAccountID, ApplicationID: p.ApplicationID, CredentialID: p.CredentialIDs[0], Environment: p.Environment, Lane: p.Lane, Authority: contract.IdentityAuthority(p.Authority), ApplicationClass: p.ApplicationClass, ApplicationIsInternal: p.ApplicationIsInternal}
	catalogue, err := LoadPublicCatalogue("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := commercial.Load("../../configs/deployment-commercial.json")
	if err != nil {
		t.Fatal(err)
	}
	live, err := openailive.New(openailive.Config{})
	if err != nil {
		t.Fatal(err)
	}
	xai, err := openairealtime.NewXAI(openairealtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(live, xai)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		route provider.Route
		quote string
		sold  bool
	}{
		{"clock-only", provider.Route{DeploymentID: "dep_live_review_fixture", Provider: "openai-live", ModelReference: "openai/gpt-live-1@observed-2026-11-14", UpstreamModelID: "gpt-live-1"}, "1.500000000000", false},
		{"existing-voice", provider.Route{DeploymentID: "dep_xai_realtime_grok_voice_3_0_observed_2026_10_26", Provider: "xai-realtime", ModelReference: "x-ai/grok-voice-3.0@observed-2026-10-26", UpstreamModelID: "grok-voice-3.0"}, "9.200000000000", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, observed := range []bool{true, false} {
				kind := contract.RealtimeLive
				observedKind := kind
				if !observed {
					observedKind = contract.RealtimeConversation
				}
				raw, _ := json.Marshal(map[string]any{"snapshotId": "reviewed-native-live", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []any{map[string]any{
					"deploymentId": tc.route.DeploymentID, "provider": tc.route.Provider, "modelReference": tc.route.ModelReference, "upstreamModelId": tc.route.UpstreamModelID, "current": true,
					"observed": map[string]any{"realtime": map[string]any{"transports": []string{"websocket"}, "sessionKinds": []contract.RealtimeSessionKind{observedKind}, "inputAudioTranscription": true, "audioFormats": map[string]any{"input": []string{"pcm16_24khz"}, "output": []string{"pcm16_24khz"}}}},
				}}})
				current, err := inventory.Parse(raw, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				models, err := catalogue.nativeModels(current, time.Now(), registry)
				if err != nil {
					t.Fatal(err)
				}
				planner, err := nativeadmission.NewPlanner(policies, models)
				if err != nil {
					t.Fatal(err)
				}
				target := contract.ModelTarget(tc.route.ModelReference)
				request := nativeadmission.Request{Operation: contract.GrantRealtimeSession, Target: &target, Facts: contract.GrantRequestFacts{RealtimeSessionKind: &kind, Stream: true}, SessionMaxDuration: 30 * time.Minute, SessionMaxResponses: 500}
				plan, err := planner.Plan(principal, request)
				if !observed {
					if !errors.Is(err, nativeadmission.ErrNoRoute) {
						t.Fatal("missing live observation admitted", err)
					}
					continue
				}
				if err != nil || plan.Quote.Amount != contract.ExactDecimal(tc.quote) {
					t.Fatal("live bound or route changed", plan.Quote, err)
				}
				if !nativeOperationServed(registry, registry, tc.route, request.Operation, &kind) {
					t.Fatal("real adapter refused live")
				}
				if nativeCommercialAllows(declarations, principal, tc.route) != tc.sold {
					t.Fatal("commercial authority changed")
				}
				if !plan.StepRoutes[0].Allows(tc.route.DeploymentID, tc.route.ModelReference, tc.route.Provider, false) {
					t.Fatal("eligible route excluded")
				}
				request.SessionMaxDuration += time.Millisecond
				if _, err := planner.Plan(principal, request); !errors.Is(err, nativeadmission.ErrInvalidRequest) {
					t.Fatal("duration cap bypassed", err)
				}
				request.SessionMaxDuration = 30 * time.Minute
				input := 128001
				request.Facts.EstimatedInputTokens = &input
				if _, err := planner.Plan(principal, request); err == nil {
					t.Fatal("context/input cap bypassed")
				}
				request.Facts.EstimatedInputTokens = nil
				if tc.name == "clock-only" {
					request.Facts.DeclaredTools = 1
					if _, err := planner.Plan(principal, request); !errors.Is(err, nativeadmission.ErrNoRoute) {
						t.Fatal("tools invented for delegated voice", err)
					}
				}
			}
		})
	}
}
