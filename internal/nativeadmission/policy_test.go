package nativeadmission

import (
	"errors"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func terms(p Policy, cap string) OperationPolicy {
	limits := clone(p.Limits)
	limits.MaxPricePerRequest = &contract.Money{Amount: contract.ExactDecimal(cap), Currency: "USD"}
	return OperationPolicy{Limits: limits, Routes: RouteScope{Mode: "eligible"}, MaxInput: 15000, AllowCatalogueModels: true}
}
func nonTextPrice(id contract.ModelID, units ...UnitPrice) ModelPrice {
	return ModelPrice{ModelLine: id, PriceVersionID: string(id) + "-v1", Currency: "USD", UnitPrices: units}
}
func TestNamedPromotionalLaneKeepsItsFallbackAndCannotWidenAuto(t *testing.T) {
	p := policy()
	premium := clone(p.Limits)
	premium.MaxPricePerRequest = &contract.Money{Amount: "1", Currency: "USD"}
	p.Promotional = &PromotionalPolicy{Levels: []contract.PowerLevel{contract.PowerPro, contract.PowerUltra}, FallbackLevel: contract.PowerXHigh, Limits: premium, Routes: RouteScope{Mode: "eligible", RequirePromotionalCredit: true}}
	expensive := model("pro")
	expensive.Price.UnitPrices[3].Amount = "50"
	r := request()
	r.Target = ptr(contract.LevelTarget(contract.PowerPro))
	planner := planner(t, p, expensive, model("high"), model("medium"))
	plan, err := planner.Plan(principal(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || *plan.Steps[0].Target.Level != contract.PowerPro || *plan.Steps[1].Target.Level != contract.PowerXHigh {
		t.Fatal("missing ordered premium fallback")
	}
	if plan.Steps[0].Limits.MaxPricePerRequest.Amount != "1" || plan.Steps[1].Limits.MaxPricePerRequest.Amount != "0.05" || !plan.StepRoutes[0].RequirePromotionalCredit || plan.StepRoutes[1].RequirePromotionalCredit {
		t.Fatal("premium widened fallback")
	}
	if plan.StepRoutes[0].Allows("any", "fixture/pro@v1", "fixture", false) {
		t.Fatal("paid route admitted as credit")
	}
	if !plan.StepRoutes[0].Allows("any", "fixture/pro@v1", "fixture", true) {
		t.Fatal("verified promo unavailable")
	}
	r.Target = ptr(contract.ModelTarget("fixture/medium@immutable"))
	exact, err := planner.Plan(principal(), r)
	if err != nil || len(exact.Steps) != 1 || exact.Steps[0].Target.Reference() != "fixture/medium@immutable" || exact.StepRoutes[0].RequirePromotionalCredit {
		t.Fatalf("exact pin inherited promotion: %+v %v", exact, err)
	}
	r.Target = ptr(contract.LevelTarget(contract.PowerAuto))
	auto, err := planner.Plan(principal(), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range auto.Steps {
		if *step.Target.Level == contract.PowerPro || step.Limits.MaxPricePerRequest.Amount != "0.05" {
			t.Fatal("auto received premium terms")
		}
	}
}
func TestImageTermsPreserveSeparatePriceAndTextBudget(t *testing.T) {
	p := policy()
	limits := clone(p.Limits)
	limits.MaxPricePerRequest = &contract.Money{Amount: "0.15", Currency: "USD"}
	p.ImageInput = &ImageInputPolicy{Limits: limits, Routes: clone(p.Routes), MaxImages: 16}
	m := model("medium")
	m.MaxContextTokens = 300000
	m.Price.UnitPrices[0].Amount = "0.1"
	m.Price.UnitPrices[2].Amount = "0.1"
	r := request()
	r.Target = ptr(contract.ModelTarget("fixture/medium"))
	r.Facts.EstimatedInputTokens = ptr(126976)
	r.Facts.InputImages = ptr(16)
	plan, err := planner(t, p, m).Plan(principal(), r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.InputTokens != 142976 || plan.Steps[0].Limits.MaxPricePerRequest.Amount != "0.15" {
		t.Fatal("image tokens were incorrectly limited by controlled-text bound")
	}
	r.Facts.InputImages = ptr(17)
	if _, err = planner(t, p, m).Plan(principal(), r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
}
func TestExactDeploymentPermissionBindsAllThreeFields(t *testing.T) {
	scope := policy().Routes
	d := scope.Deployments[0]
	if !scope.Allows(d.DeploymentID, d.ModelReference, d.Provider, false) {
		t.Fatal("positive control")
	}
	if scope.Allows(d.DeploymentID, "fixture/another@v1", d.Provider, false) || scope.Allows(d.DeploymentID, d.ModelReference, "another", false) || scope.Allows("another", d.ModelReference, d.Provider, false) {
		t.Fatal("deployment tuple widened")
	}
	if (RouteScope{Mode: "exact"}).Allows(d.DeploymentID, d.ModelReference, d.Provider, false) {
		t.Fatal("empty exact scope means unrestricted")
	}
}
func TestSpeechCharacterCeilingAndBatchTranscriptionDuration(t *testing.T) {
	p := policy()
	speech := terms(p, "0.25")
	p.Speech = &speech
	voice := model("speech")
	voice.APIFormats = []string{"audio_speech"}
	voice.OutputModalities = []string{"audio"}
	voice.Price = nonTextPrice(voice.ID, UnitPrice{contract.UnitCharacters, "15", 1000000, "USD"}, UnitPrice{contract.UnitRequests, "0", 1, "USD"})
	r := Request{Operation: contract.GrantAudioSpeech, Target: ptr(contract.ModelTarget("fixture/speech")), Facts: contract.GrantRequestFacts{InputCharacters: ptr(15000)}}
	plan, err := planner(t, p, voice).Plan(principal(), r)
	if err != nil || plan.Quote.Amount != "0.225000000000" {
		t.Fatalf("speech quote: %+v %v", plan, err)
	}
	r.Facts.InputCharacters = ptr(15001)
	if _, err = planner(t, p, voice).Plan(principal(), r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	transcription := terms(p, "0.05")
	transcription.MaxInput = 300000
	p.Transcription = &transcription
	batch := model("transcribe")
	batch.APIFormats = []string{"audio_transcriptions"}
	batch.InputModalities = []string{"audio"}
	batch.Price = nonTextPrice(batch.ID, UnitPrice{contract.UnitAudioInputMilliseconds, "0.1", 3600000, "USD"})
	r = Request{Operation: contract.GrantAudioTranscriptions, Target: ptr(contract.ModelTarget("fixture/transcribe")), Facts: contract.GrantRequestFacts{AudioInputMilliseconds: ptr(300000)}}
	plan, err = planner(t, p, batch).Plan(principal(), r)
	if err != nil || plan.Quote.Amount != "0.008333333334" {
		t.Fatalf("transcription quote: %+v %v", plan, err)
	}
	batch.Price.UnitPrices = append(batch.Price.UnitPrices, UnitPrice{contract.UnitOutputTokens, "1", 1000000, "USD"})
	if _, err = planner(t, p, batch).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatal("unbounded priced transcript admitted")
	}
}
func TestRealtimeMinuteQualificationAndWholeSessionBound(t *testing.T) {
	p := policy()
	realtime := terms(p, "0.25")
	realtime.MaxInput = 126976
	realtime.MaxSessionDuration = 30 * time.Minute
	realtime.MaxSessionResponses = 500
	realtime.SessionKinds = []contract.RealtimeSessionKind{"conversation", "transcription"}
	p.Realtime = &realtime
	voice := model("realtime")
	voice.RealtimeSessionKinds = []contract.RealtimeSessionKind{"conversation"}
	voice.InputModalities = []string{"text", "audio"}
	voice.OutputModalities = []string{"text", "audio"}
	voice.Price = nonTextPrice(voice.ID, UnitPrice{contract.UnitAudioInputMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitAudioOutputMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitSessionMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitRequests, "0.004", 1, "USD"})
	r := Request{Operation: contract.GrantRealtimeSession, Target: ptr(contract.ModelTarget("fixture/realtime")), Facts: contract.GrantRequestFacts{EstimatedInputTokens: ptr(1000), RealtimeSessionKind: ptr(contract.RealtimeSessionKind("conversation")), DeclaredTools: 1}, SessionMaxDuration: 30 * time.Minute, SessionMaxResponses: 500}
	plan, err := planner(t, p, voice).Plan(principal(), r)
	if err != nil || plan.Quote.Amount != "9.200000000000" || plan.ExecutionTimeout != 30*time.Minute {
		t.Fatalf("session bound: %+v %v", plan, err)
	}
	voice.Price.UnitPrices[0].Amount = "0.1"
	if _, err = planner(t, p, voice).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatal("over per-minute cap admitted")
	}
	voice.Price.UnitPrices[0].Amount = "0.08"
	r.SessionMaxResponses = 501
	if _, err = planner(t, p, voice).Plan(principal(), r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("unbounded response fee admitted")
	}
	r.SessionMaxResponses = 500
	voice.Price.UnitPrices = append(voice.Price.UnitPrices, UnitPrice{contract.UnitOutputTokens, "1", 1000000, "USD"})
	if _, err = planner(t, p, voice).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatal("token-priced realtime admitted without token bound")
	}
}

func TestNativePopulationRequiresExactReviewedCredential(t *testing.T) {
	p := policy()
	known := principal()
	if !p.Selects(known) {
		t.Fatal("reviewed workload omitted")
	}
	for _, credential := range []contract.CredentialID{"service-secret-same-application", "new-unreviewed-workload"} {
		other := known
		other.CredentialID = credential
		if p.Selects(other) {
			t.Fatal("application identity widened credential population")
		}
		if _, err := planner(t, p, model("medium")).Plan(other, request()); !errors.Is(err, ErrNotEnrolled) {
			t.Fatalf("unreviewed credential admitted: %v", err)
		}
	}
	p.Enabled = false
	if !p.Selects(known) {
		t.Fatal("disabled population escaped native selection")
	}
	if _, err := planner(t, p, model("medium")).Plan(known, request()); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("disabled population executed: %v", err)
	}
}
