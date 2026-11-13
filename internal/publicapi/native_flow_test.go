package publicapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func nativePtr[T any](v T) *T { return &v }

type nativeIdentity struct {
	principal contract.IdentityPrincipal
	fail      bool
}

func (i *nativeIdentity) Verify(_ context.Context, r contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError) {
	if i.fail {
		return nil, contract.NewPublicError(r.RequestID, "authentication_failed", "identity rejected")
	}
	return &contract.IdentityAssertion{Principal: i.principal, UserID: r.OnBehalfOfUserID}, nil
}

type nativeMemory struct {
	mu                                                      sync.Mutex
	admission                                               *nativeadmission.Admission
	staged, settled, bound                                  int
	admitErr, stageErr, settleErr, lookupErr, generationErr error
	known                                                   bool
}

func (s *nativeMemory) Admit(_ context.Context, r nativeadmission.AdmissionRequest) (*nativeadmission.Admission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admitErr != nil {
		return nil, s.admitErr
	}
	s.admission = &nativeadmission.Admission{ID: "kad_test", RequestID: r.RequestID, Principal: r.Principal, UserID: r.UserID, Operation: r.Operation, Plan: r.Plan, LeaseUntil: time.Now().Add(r.Plan.ExecutionTimeout)}
	return s.admission, nil
}
func (s *nativeMemory) BindGeneration(_ context.Context, _ string, _ contract.GenerationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound++
	return nil
}
func (s *nativeMemory) HasNativeGeneration(context.Context, contract.CredentialID, *contract.GenerationID, *contract.IdempotencyKey) (bool, error) {
	return s.known, s.lookupErr
}
func (s *nativeMemory) Generation(context.Context, contract.IdentityPrincipal, *contract.GenerationID, *contract.IdempotencyKey, *contract.UserID) (json.RawMessage, error) {
	if s.generationErr != nil {
		return nil, s.generationErr
	}
	return json.RawMessage(chargedRecord), nil
}
func (s *nativeMemory) StageCompletion(context.Context, string, contract.UsageReport, []providercost.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staged++
	return s.stageErr
}
func (s *nativeMemory) Settle(context.Context, string, contract.UsageReport, []providercost.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled++
	return s.settleErr
}
func (s *nativeMemory) ReplayCompletions(context.Context, int) (int, error) { return 0, nil }

func nativeFixture(t *testing.T) (*publicapi.NativeConfig, *nativeMemory, *publicapi.PublicCatalogue) {
	t.Helper()
	identity := &nativeIdentity{principal: contract.IdentityPrincipal{OwnerAccountID: nativePtr(contract.AccountID("owner")), ApplicationID: "app", CredentialID: "workload_handle", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.IdentityAuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}}
	policy := nativeadmission.Policy{EconomicTreatment: nativeadmission.InternalMetered, ID: "native-fixture", Version: 1, Enabled: true, CredentialIDs: []contract.CredentialID{"workload_handle"}, OwnerAccountID: "owner", ApplicationID: "app", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.ServiceAuthority("first_party"), ApplicationClass: "internal", ApplicationIsInternal: true, Audience: contract.AudiencePlatformInternal, AllowedLevels: []contract.PowerLevel{contract.PowerAuto, contract.PowerHigh}, AllowedModels: []contract.ModelID{textModel.ModelID()}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: "dep_stub_b", Provider: "stub", ModelReference: textModel}}}, Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.05", Currency: "USD"}, MaxFundingTier: nativePtr(contract.FundingTierCredit), RequireZeroDataRetention: true, ProhibitTrainingOnCustomerData: true}, MaxInputTokens: 126976, MaxOutputTokens: 8192, DefaultOutputTokens: 8192, MaxTools: 128, MaxConcurrent: 32, MaxPerUTCDay: 5000, ExecutionTimeout: time.Minute}
	var document map[string]any
	if err := json.Unmarshal([]byte(declaredCatalogue), &document); err != nil {
		t.Fatal(err)
	}
	for _, item := range document["models"].([]any) {
		model := item.(map[string]any)
		units := []any{map[string]any{"unit": "cached_input_tokens", "amount": "0.000001", "per": 1, "currency": "USD"}, map[string]any{"unit": "reasoning_tokens", "amount": "0.000002", "per": 1, "currency": "USD"}, map[string]any{"unit": "input_tokens", "amount": "0.000001", "per": 1, "currency": "USD"}, map[string]any{"unit": "output_tokens", "amount": "0.000002", "per": 1, "currency": "USD"}, map[string]any{"unit": "requests", "amount": "0", "per": 1, "currency": "USD"}}
		if model["modelId"] == string(realtimeModel.ModelID()) {
			units = []any{map[string]any{"unit": "audio_input_milliseconds", "amount": "0", "per": 1, "currency": "USD"}, map[string]any{"unit": "audio_output_milliseconds", "amount": "0", "per": 1, "currency": "USD"}, map[string]any{"unit": "session_milliseconds", "amount": "0.08", "per": 60000, "currency": "USD"}}
		}
		model["pricing"] = map[string]any{"priceVersionId": model["modelId"].(string) + "-native-v1", "currency": "USD", "unitPrices": units}
	}
	raw, _ := json.Marshal(document)
	catalogue, err := publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	store := &nativeMemory{}
	return &publicapi.NativeConfig{Identity: identity, Store: store, Policies: []nativeadmission.Policy{policy}}, store, catalogue
}

func TestNativeTextHTTPUsesDurableAdmissionWithoutEconomicProxy(t *testing.T) {
	for _, dialect := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/responses", map[string]any{"model": "auto", "input": "hello", "stream": true}},
		{"/v1/chat/completions", map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": true}},
		{"/v1/messages", map[string]any{"model": "auto", "max_tokens": 8192, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": true}},
	} {
		t.Run(dialect.path, func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.post(dialect.path, validKey, dialect.body)
			if response.StatusCode != 200 {
				t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
			}
			if call := h.adapter.lastCall(); call == nil || call.MaxOutputTokens == nil || *call.MaxOutputTokens != 8192 {
				t.Fatal("admitted output bound not applied")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.staged != 1 || store.settled != 1 || store.bound != 1 {
				t.Fatalf("durability: staged=%d settled=%d bound=%d", store.staged, store.settled, store.bound)
			}
			h.oxy.mu.Lock()
			defer h.oxy.mu.Unlock()
			if h.oxy.grants != 0 || h.oxy.deliveries != 0 {
				t.Fatal("native request reached financial proxy")
			}
		})
	}
}

func TestNativeFailuresNeverRetryThroughLegacyAuthority(t *testing.T) {
	for _, kind := range []string{"identity", "capacity", "disabled", "legacy_key", "stage", "settle"} {
		t.Run(kind, func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			switch kind {
			case "identity":
				config.Identity.(*nativeIdentity).fail = true
			case "capacity":
				store.admitErr = nativeadmission.ErrCapacity
			case "disabled":
				store.admitErr = nativeadmission.ErrDisabled
			case "legacy_key":
				store.admitErr = nativeadmission.ErrLegacy
			case "stage":
				store.stageErr = errors.New("storage unavailable")
			case "settle":
				store.settleErr = errors.New("settlement pending")
			}
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.post("/v1/responses", validKey, map[string]any{"model": "auto", "input": "hello", "stream": true})
			if kind == "stage" {
				if !strings.Contains(string(response.Body), "service_unavailable") || strings.Contains(string(response.Body), `"type":"done"`) {
					t.Fatalf("stage failure reported success: %s", response.Body)
				}
			} else if kind == "settle" {
				if !strings.Contains(string(response.Body), `"type":"done"`) {
					t.Fatalf("durably staged response did not complete: %s", response.Body)
				}
			} else if response.StatusCode == 200 || h.adapter.called() != 0 {
				t.Fatal("failed admission executed")
			}
			h.oxy.mu.Lock()
			defer h.oxy.mu.Unlock()
			if h.oxy.grants != 0 || h.oxy.deliveries != 0 {
				t.Fatal("native failure reached legacy finances")
			}
		})
	}
}

func TestNativeGenerationReadOnlyFallsBackOnConfirmedAbsence(t *testing.T) {
	for _, kind := range []string{"native", "pending", "foreign", "database", "identity", "historical"} {
		t.Run(kind, func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			store.known = true
			switch kind {
			case "pending", "foreign":
				store.generationErr = nativeadmission.ErrNotFound
			case "database":
				store.lookupErr = errors.New("database unavailable")
			case "identity":
				config.Identity.(*nativeIdentity).fail = true
			case "historical":
				store.known = false
			}
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.get("/v1/generations/gen_1", validKey)
			if kind == "native" || kind == "historical" {
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%d %s", response.StatusCode, response.Body)
				}
			} else if response.StatusCode == http.StatusOK {
				t.Fatal("foreign/pending/error exposed legacy record")
			}
			calls := h.oxy.authorizationBodies()
			if kind == "historical" {
				if len(calls) != 1 || !strings.Contains(calls[0], `"kind":"receipt.read"`) {
					t.Fatal("historical read did not use original read-only owner")
				}
			} else if len(calls) != 0 {
				t.Fatal("native read escaped ownership/error boundary")
			}
		})
	}
}

func TestNativeRealtimeCompletionIsDurableBeforeClosed(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "storage_failure"}[failed], func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			p := &config.Policies[0]
			p.AllowedModels = append(p.AllowedModels, realtimeModel.ModelID())
			p.Realtime = &nativeadmission.OperationPolicy{Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.25", Currency: "USD"}, MaxFundingTier: nativePtr(contract.FundingTierPaid)}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: "dep_realtime", ModelReference: realtimeModel, Provider: "openai-realtime"}}}, MaxInput: 126976, MaxSessionDuration: 30 * time.Minute, MaxSessionResponses: 500, SessionKinds: []contract.RealtimeSessionKind{contract.RealtimeConversation}}
			if failed {
				store.stageErr = errors.New("durability unavailable")
			}
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue, realtimeFunding: provider.FundingPaid})
			open := conversationOpen(nil)
			open["limits"] = map[string]any{"maxDurationMs": 120000, "maxResponses": 500}
			ws := h.dialRealtime(validKey, open)
			created := readEvent(t, ws)
			if created.head.Type != "session.created" {
				t.Fatalf("not admitted: %s", created.raw)
			}
			sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": created.head.RequestID, "commandId": "close-native", "type": "session.close"})
			for {
				event := readEvent(t, ws)
				if event.head.Type == "session.closed" {
					if failed {
						t.Fatal("unpersisted completion reported closed")
					}
					break
				}
				if event.head.Type == "error" {
					if !failed || !strings.Contains(string(event.raw), "service_unavailable") {
						t.Fatalf("unexpected failure %s", event.raw)
					}
					break
				}
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.staged != 1 || (!failed && store.settled != 1) || (failed && store.settled != 0) {
				t.Fatalf("completion boundary stage%d settle%d", store.staged, store.settled)
			}
			if store.admission.Plan.SessionMaxResponses != 500 || store.admission.Plan.SessionMaxDuration != 2*time.Minute {
				t.Fatal("session bounds absent from native admission")
			}
			if len(h.oxy.authorizationBodies()) != 0 {
				t.Fatal("native session used Oxy economic authorization")
			}
		})
	}
}

func nativeAudioFixture(t *testing.T) (*publicapi.NativeConfig, *nativeMemory, *publicapi.PublicCatalogue) {
	t.Helper()
	config, store, catalogue := nativeFixture(t)
	var doc map[string]any
	if err := json.Unmarshal(catalogue.Document(), &doc); err != nil {
		t.Fatal(err)
	}
	models := doc["models"].([]any)
	for _, entry := range models {
		model := entry.(map[string]any)
		if model["modelId"] == string(speechModel.ModelID()) {
			model["pricing"] = map[string]any{"priceVersionId": "native-speech-v1", "currency": "USD", "unitPrices": []any{map[string]any{"unit": "characters", "amount": "0.000015", "per": 1, "currency": "USD"}}}
		}
	}
	cloned, _ := json.Marshal(models[0])
	var transcription map[string]any
	_ = json.Unmarshal(cloned, &transcription)
	transcription["modelId"] = string(whisperModel.ModelID())
	caps := transcription["capabilities"].(map[string]any)
	caps["inputModalities"] = []string{"audio"}
	caps["outputModalities"] = []string{"text"}
	transcription["pricing"] = map[string]any{"priceVersionId": "native-transcription-v1", "currency": "USD", "unitPrices": []any{map[string]any{"unit": "audio_input_milliseconds", "amount": "0.005", "per": 60000, "currency": "USD"}}}
	doc["models"] = append(models, transcription)
	raw, _ := json.Marshal(doc)
	var err error
	catalogue, err = publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := &config.Policies[0]
	p.AllowedModels = append(p.AllowedModels, speechModel.ModelID(), whisperModel.ModelID())
	terms := func(id contract.DeploymentID, model contract.ModelReference, cap string, maxInput int) *nativeadmission.OperationPolicy {
		return &nativeadmission.OperationPolicy{Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: contract.ExactDecimal(cap), Currency: "USD"}, MaxFundingTier: nativePtr(contract.FundingTierPaid)}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: id, ModelReference: model, Provider: "stub"}}}, MaxInput: maxInput}
	}
	p.Speech = terms("dep_speech", speechModel, "0.25", 15000)
	p.Transcription = terms("dep_whisper", whisperModel, "0.05", 300000)
	return config, store, catalogue
}
func TestNativeSpeechAndTranscriptionUseServedAdapterCapabilities(t *testing.T) {
	for _, operation := range []string{"speech", "transcriptions"} {
		t.Run(operation, func(t *testing.T) {
			config, store, catalogue := nativeAudioFixture(t)
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			body := map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}
			if operation == "transcriptions" {
				body = map[string]any{"model": "stub/whisper", "audio": map[string]any{"mediaType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wav(1500))}}
			}
			response := h.post("/v1/audio/"+operation, validKey, body)
			if response.StatusCode != 200 {
				t.Fatalf("voice request failed %d %s", response.StatusCode, response.Body)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.staged != 1 || store.settled != 1 || h.adapter.called() != 1 {
				t.Fatal("voice did not execute and persist exactly once")
			}
			if len(h.oxy.authorizationBodies()) != 0 {
				t.Fatal("voice used old economic proxy")
			}
		})
	}
	for _, reason := range []string{"unsupported_adapter", "tuple_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			config, store, catalogue := nativeAudioFixture(t)
			options := harnessOptions{native: config, catalogue: catalogue}
			if reason == "unsupported_adapter" {
				options.stubFormats = []contract.APIFormat{contract.APIFormatResponses}
			} else {
				config.Policies[0].Speech.Routes.Deployments[0].DeploymentID = "dep_other"
			}
			h := newHarness(t, options)
			response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
			if response.StatusCode == 200 || h.adapter.called() != 0 || store.admission != nil || len(h.oxy.authorizationBodies()) != 0 {
				t.Fatal("unsupported voice route reached admission/provider/old authority")
			}
		})
	}
}
